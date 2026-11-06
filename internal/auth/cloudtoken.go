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
	providers map[string]*issuerKeys // by issuer URL
	cache     map[[32]byte]cachedID
}

type cachedID struct {
	id  *Identity
	exp time.Time
}

func NewCloudValidator(pool *pgxpool.Pool) *CloudValidator {
	return &CloudValidator{
		Pool:      pool,
		providers: map[string]*issuerKeys{},
		cache:     map[[32]byte]cachedID{},
	}
}

// Verified is what a verified identity token says about its caller.
type Verified struct {
	Issuer *snapshot.Issuer
	Email  string // the email claim, lowercased
	Expiry time.Time
}

// TokenError is why an identity token was refused. Reason is short and safe
// to show the caller ("expired", "wrong audience"); Err has the detail, for
// the log. Unavailable marks a failure on our side: the issuer's discovery
// document or keys could not be fetched, so the token may well be good.
type TokenError struct {
	Reason      string
	Unavailable bool
	Err         error
}

func (e *TokenError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *TokenError) Unwrap() error { return e.Err }

// TokenErrorReason is the part of an identity-token error a caller may see.
func TokenErrorReason(err error) string {
	if te, ok := errors.AsType[*TokenError](err); ok {
		return te.Reason
	}
	return "invalid token"
}

// IssuerUnavailable reports whether err is the issuer's discovery or keys
// failing to load, rather than the token being refused.
func IssuerUnavailable(err error) bool {
	te, ok := errors.AsType[*TokenError](err)
	return ok && te.Unavailable
}

func refused(reason string, err error) *TokenError { return &TokenError{Reason: reason, Err: err} }

// Verify checks a token against the configured issuer it names: the signature
// against the issuer's published keys, expiry, audience, the issuer's match
// claims and its email claim. It looks no user up and creates none. Its errors
// are *TokenError.
func (v *CloudValidator) Verify(ctx context.Context, s *snapshot.Snapshot, token string) (*Verified, error) {
	if len(s.Issuers) == 0 {
		return nil, refused("no token issuers configured", nil)
	}
	iss, err := unverifiedIssuer(token)
	if err != nil {
		return nil, refused("malformed token", err)
	}
	var issuer *snapshot.Issuer
	for _, cand := range s.Issuers {
		if cand.IssuerURL == iss {
			issuer = cand
			break
		}
	}
	if issuer == nil {
		return nil, refused("unknown issuer", fmt.Errorf("issuer %s is not configured", iss))
	}

	ik, err := v.issuerKeys(ctx, issuer.IssuerURL)
	if err != nil {
		return nil, &TokenError{Reason: "issuer unavailable", Unavailable: true,
			Err: fmt.Errorf("issuer discovery failed: %w", err)}
	}
	keys := &watchedKeys{inner: ik.keys}
	verifier := oidc.NewVerifier(issuer.IssuerURL, keys, &oidc.Config{ClientID: issuer.Audience, SupportedSigningAlgs: ik.algs})
	idToken, err := verifier.Verify(ctx, token)
	if err != nil {
		if keys.fetchErr != nil {
			return nil, &TokenError{Reason: "issuer unavailable", Unavailable: true,
				Err: fmt.Errorf("issuer keys: %w", keys.fetchErr)}
		}
		return nil, refused(verifyReason(err), err)
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, refused("malformed token", err)
	}
	for claim, want := range issuer.Match {
		if got, _ := claims[claim].(string); got != want {
			return nil, refused("claim mismatch", fmt.Errorf("claim %s mismatch", claim))
		}
	}
	emailClaim := issuer.EmailClaim
	email, _ := claims[emailClaim].(string)
	if email == "" {
		return nil, refused("no email claim", fmt.Errorf("token has no %s claim", emailClaim))
	}
	return &Verified{Issuer: issuer, Email: strings.ToLower(email), Expiry: idToken.Expiry}, nil
}

// verifyReason names what go-oidc's verifier refused, for the caller.
func verifyReason(err error) string {
	if _, ok := errors.AsType[*oidc.TokenExpiredError](err); ok {
		return "expired"
	}
	msg := err.Error()
	for _, r := range []struct{ has, reason string }{
		{"expected audience", "wrong audience"},
		{"before the nbf", "not valid yet"},
		{"issued by a different provider", "unknown issuer"},
		{"unsupported algorithm", "unsupported signing algorithm"},
		{"failed to verify signature", "bad signature"},
		{"malformed", "malformed token"},
	} {
		if strings.Contains(msg, r.has) {
			return r.reason
		}
	}
	return "invalid token"
}

// issuerKeys is what verifying an issuer's tokens needs: its key set, from
// the jwks_uri it publishes, and the algorithms it signs with.
type issuerKeys struct {
	keys *oidc.RemoteKeySet
	algs []string
}

// watchedKeys records whether fetching the issuer's keys failed during one
// verification: go-oidc reports that as a signature failure.
type watchedKeys struct {
	inner    oidc.KeySet
	fetchErr error
}

func (w *watchedKeys) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := w.inner.VerifySignature(ctx, jwt)
	// RemoteKeySet wraps only a failed fetch ("fetching keys %w"); a token no
	// key verifies, or a malformed one, is a plain error.
	if err != nil && errors.Unwrap(err) != nil {
		w.fetchErr = err
	}
	return payload, err
}

// Validate implements gateway.CloudTokenValidator. Its errors are *TokenError.
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
			return nil, refused("no gateway user for "+email, nil)
		}
		created, err := v.ProvisionUser(ctx, email, issuer.MapToTeam)
		if errors.Is(err, ErrUserDisabled) {
			return nil, refused(email+" is disabled", err)
		}
		if err != nil {
			return nil, &TokenError{Reason: "could not create a user for " + email, Unavailable: true, Err: err}
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

// issuerKeys discovers an issuer once. Discovery runs outside the lock, so a
// slow issuer doesn't hold up cached tokens or other issuers.
func (v *CloudValidator) issuerKeys(ctx context.Context, issuerURL string) (*issuerKeys, error) {
	v.mu.Lock()
	ik := v.providers[issuerURL]
	v.mu.Unlock()
	if ik != nil {
		return ik, nil
	}
	p, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	var d struct {
		JWKSURL string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := p.Claims(&d); err != nil {
		return nil, err
	}
	// What the issuer says it signs with, of what go-oidc verifies; RS256 when
	// it doesn't say (NewVerifier's default).
	var algs []string
	for _, a := range d.Algs {
		switch a {
		case oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512,
			oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA:
			algs = append(algs, a)
		}
	}
	ik = &issuerKeys{keys: oidc.NewRemoteKeySet(context.Background(), d.JWKSURL), algs: algs}
	v.mu.Lock()
	defer v.mu.Unlock()
	if cur := v.providers[issuerURL]; cur != nil {
		return cur, nil
	}
	v.providers[issuerURL] = ik
	return ik, nil
}

// ErrUserDisabled is returned for a disabled account: an identity token never
// brings one back.
var ErrUserDisabled = errors.New("user is disabled")

// ProvisionUser creates the user for a verified email on first use, joined to
// teamID when one is set, and bumps the config version so the control plane
// and every gateway get them. An account that already exists is returned with
// its memberships unchanged; a disabled one is ErrUserDisabled. The returned
// user carries their team ids.
//
// The user and their team membership are written in one transaction, so a
// failed join leaves no user behind, and a concurrent first call for the same
// email waits on the email's unique index until that transaction commits:
// every caller sees the user with their team.
func (v *CloudValidator) ProvisionUser(ctx context.Context, email, teamID string) (*snapshot.User, error) {
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var userID string
	var disabled, created bool
	err = tx.QueryRow(ctx,
		`INSERT INTO users (id, email, role) VALUES ($1,$2,'member') ON CONFLICT (email) DO NOTHING RETURNING id`,
		uuid.New(), email).Scan(&userID)
	switch {
	case err == nil:
		created = true
	case errors.Is(err, pgx.ErrNoRows):
		// users.email is citext, so this finds the account whatever its case.
		if err := tx.QueryRow(ctx, `SELECT id, disabled FROM users WHERE email=$1`, email).Scan(&userID, &disabled); err != nil {
			return nil, fmt.Errorf("auto-provision failed: %w", err)
		}
	default:
		return nil, fmt.Errorf("auto-provision failed: %w", err)
	}
	if disabled {
		return nil, ErrUserDisabled
	}
	if created && teamID != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,'member') ON CONFLICT DO NOTHING`,
			teamID, userID); err != nil {
			return nil, fmt.Errorf("auto-provision failed: joining team %s: %w", teamID, err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT team_id::text FROM team_members WHERE user_id=$1 ORDER BY team_id`, userID)
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("auto-provision failed: %w", err)
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
