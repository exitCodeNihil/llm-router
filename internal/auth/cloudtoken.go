package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// CloudValidator verifies inbound Azure Entra ID / GCP identity tokens against
// the issuers configured in the console, and maps their claims to a gateway
// user. Callers on Azure use `az account get-access-token --resource <audience>`;
// callers on GCP use `gcloud auth print-identity-token --audiences=<audience>`.
type CloudValidator struct {
	// Pool enables auto-provisioning of first-seen users; nil turns it off.
	Pool *pgxpool.Pool

	mu        sync.Mutex
	providers map[string]*oidc.Provider // issuer URL -> discovered provider
	cache     map[[32]byte]cachedID
}

type cachedID struct {
	id  *Identity
	exp time.Time
}

func NewCloudValidator(pool *pgxpool.Pool) *CloudValidator {
	return &CloudValidator{
		Pool:      pool,
		providers: map[string]*oidc.Provider{},
		cache:     map[[32]byte]cachedID{},
	}
}

// Verified is what a verified identity token says about its caller.
type Verified struct {
	Issuer *snapshot.Issuer
	Email  string // the email claim, lowercased
	Expiry time.Time
}

// Verify checks a token against the configured issuer it names: the signature
// against the issuer's published keys, expiry, audience, the issuer's match
// claims and its email claim. It looks no user up and creates none.
func (v *CloudValidator) Verify(ctx context.Context, s *snapshot.Snapshot, token string) (*Verified, error) {
	if len(s.Issuers) == 0 {
		return nil, fmt.Errorf("no token issuers configured")
	}
	iss, err := unverifiedIssuer(token)
	if err != nil {
		return nil, err
	}
	var issuer *snapshot.Issuer
	for _, cand := range s.Issuers {
		if cand.IssuerURL == iss {
			issuer = cand
			break
		}
	}
	if issuer == nil {
		return nil, fmt.Errorf("issuer %s is not configured", iss)
	}

	provider, err := v.provider(ctx, issuer.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("issuer discovery failed: %w", err)
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: issuer.Audience}).Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	for claim, want := range issuer.Match {
		if got, _ := claims[claim].(string); got != want {
			return nil, fmt.Errorf("claim %s mismatch", claim)
		}
	}
	emailClaim := issuer.EmailClaim
	email, _ := claims[emailClaim].(string)
	if email == "" {
		return nil, fmt.Errorf("token has no %s claim", emailClaim)
	}
	return &Verified{Issuer: issuer, Email: strings.ToLower(email), Expiry: idToken.Expiry}, nil
}

// Validate implements gateway.CloudTokenValidator.
func (v *CloudValidator) Validate(ctx context.Context, s *snapshot.Snapshot, token string) (*Identity, error) {
	tokenHash := sha256.Sum256([]byte(token))
	v.mu.Lock()
	if c, ok := v.cache[tokenHash]; ok && time.Now().Before(c.exp) {
		v.mu.Unlock()
		return c.id, nil
	}
	v.mu.Unlock()

	vt, err := v.Verify(ctx, s, token)
	if err != nil {
		return nil, err
	}
	issuer, email := vt.Issuer, vt.Email
	user := s.UsersByEmail[email]
	if user == nil {
		if !issuer.AutoCreateUser || v.Pool == nil {
			return nil, fmt.Errorf("no gateway user for %s", email)
		}
		created, err := v.ProvisionUser(ctx, email, issuer.MapToTeam)
		if err != nil {
			return nil, err
		}
		user = created
	}
	id := &Identity{User: user}
	if issuer.MapToTeam != "" {
		id.Team = s.TeamsByID[issuer.MapToTeam]
	}

	exp := vt.Expiry
	if max := time.Now().Add(5 * time.Minute); exp.After(max) {
		exp = max
	}
	v.mu.Lock()
	if len(v.cache) > 10000 { // ponytail: crude cap; LRU if this ever matters
		clear(v.cache)
	}
	v.cache[tokenHash] = cachedID{id: id, exp: exp}
	v.mu.Unlock()
	return id, nil
}

func (v *CloudValidator) provider(ctx context.Context, issuerURL string) (*oidc.Provider, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p, ok := v.providers[issuerURL]; ok {
		return p, nil
	}
	p, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	v.providers[issuerURL] = p
	return p, nil
}

// ErrUserDisabled is returned for a disabled account: an identity token never
// brings one back.
var ErrUserDisabled = errors.New("user is disabled")

// ProvisionUser creates the user for a verified email on first use, joined to
// teamID when one is set, and bumps the config version so the control plane
// and every gateway get them. An account that already exists is returned with
// its memberships unchanged; a disabled one is ErrUserDisabled. The returned
// user carries their team ids.
func (v *CloudValidator) ProvisionUser(ctx context.Context, email, teamID string) (*snapshot.User, error) {
	// users.email is citext, so this finds the account whatever its case.
	const find = `SELECT id, disabled FROM users WHERE email=$1`
	var userID string
	var disabled bool
	created := false
	err := v.Pool.QueryRow(ctx, find, email).Scan(&userID, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		tag, err := v.Pool.Exec(ctx,
			`INSERT INTO users (id, email, role) VALUES ($1,$2,'member') ON CONFLICT (email) DO NOTHING`,
			uuid.New(), email)
		if err != nil {
			return nil, fmt.Errorf("auto-provision failed: %w", err)
		}
		created = tag.RowsAffected() == 1
		if err := v.Pool.QueryRow(ctx, find, email).Scan(&userID, &disabled); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if disabled {
		return nil, ErrUserDisabled
	}
	if teamID != "" && created {
		if _, err := v.Pool.Exec(ctx,
			`INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,'member') ON CONFLICT DO NOTHING`,
			teamID, userID); err != nil {
			slog.Warn("auto-provision team join failed", "team", teamID, "err", err)
		}
	}
	rows, err := v.Pool.Query(ctx, `SELECT team_id::text FROM team_members WHERE user_id=$1 ORDER BY team_id`, userID)
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if created {
		if err := (&store.Store{Pool: v.Pool}).BumpConfigVersion(ctx); err != nil {
			slog.Error("bump config version after auto-provision failed", "err", err)
		}
		slog.Info("auto-provisioned user from cloud token", "email", email)
	}
	return &snapshot.User{ID: userID, Email: email, Role: "member", TeamIDs: teams}, nil
}

// unverifiedIssuer reads the iss claim without verifying the signature, to
// pick which configured issuer to verify against.
func unverifiedIssuer(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("undecodable JWT payload")
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("unparseable JWT payload")
	}
	return claims.Iss, nil
}
