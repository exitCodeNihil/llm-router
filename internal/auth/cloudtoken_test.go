package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// fakeIssuer serves an OIDC discovery document and JWKS, and mints signed JWTs.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"jwks_uri":                              f.srv.URL + "/jwks",
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &f.key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	base := map[string]any{
		"iss": f.srv.URL,
		"aud": "api://llm-router",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
		"sub": "subject-1",
	}
	for k, v := range claims {
		base[k] = v
	}
	payload, _ := json.Marshal(base)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func testSnapshot(iss *fakeIssuer) *snapshot.Snapshot {
	return &snapshot.Snapshot{
		UsersByEmail: map[string]*snapshot.User{
			"dev@example.com": {ID: "u1", Email: "dev@example.com"},
		},
		TeamsByID: map[string]*snapshot.Team{"t1": {ID: "t1", Name: "platform"}},
		Issuers: []*snapshot.Issuer{{
			ID: "i1", Type: "entra", IssuerURL: iss.srv.URL, Audience: "api://llm-router",
			EmailClaim: "preferred_username",
			Match:      map[string]string{"tid": "tenant-1"},
			MapToTeam:  "t1",
		}},
	}
}

func TestCloudTokenValidation(t *testing.T) {
	iss := newFakeIssuer(t)
	s := testSnapshot(iss)
	v := NewCloudValidator(nil)
	ctx := context.Background()

	good := iss.mint(t, map[string]any{"preferred_username": "Dev@Example.com", "tid": "tenant-1"})
	id, err := v.Validate(ctx, s, good)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if id.User == nil || id.User.ID != "u1" || id.Team == nil || id.Team.ID != "t1" {
		t.Fatalf("wrong identity mapping: %+v", id)
	}

	// cached second call
	if _, err := v.Validate(ctx, s, good); err != nil {
		t.Fatalf("cached validation failed: %v", err)
	}

	cases := map[string]string{
		"wrong tenant":   iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "evil"}),
		"wrong audience": iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1", "aud": "api://other"}),
		"unknown user":   iss.mint(t, map[string]any{"preferred_username": "stranger@example.com", "tid": "tenant-1"}),
		"missing email":  iss.mint(t, map[string]any{"tid": "tenant-1"}),
		"expired":        iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1", "exp": time.Now().Add(-time.Hour).Unix()}),
		"garbage":        "not.a.jwt",
	}
	for name, tok := range cases {
		if _, err := v.Validate(ctx, s, tok); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	// unknown issuer
	other := newFakeIssuer(t)
	if _, err := v.Validate(ctx, s, other.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1"})); err == nil {
		t.Error("token from unconfigured issuer must be rejected")
	}
}

// TestCloudTokenErrors: refusals carry a short reason without URLs, and an
// issuer that can't be reached is told apart from a bad token.
func TestCloudTokenErrors(t *testing.T) {
	iss := newFakeIssuer(t)
	s := testSnapshot(iss)
	v := NewCloudValidator(nil)
	ctx := context.Background()
	claims := func(extra map[string]any) map[string]any {
		c := map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1"}
		for k, val := range extra {
			c[k] = val
		}
		return c
	}
	for want, tok := range map[string]string{
		"expired":         iss.mint(t, claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})),
		"wrong audience":  iss.mint(t, claims(map[string]any{"aud": "api://other"})),
		"claim mismatch":  iss.mint(t, claims(map[string]any{"tid": "evil"})),
		"no email claim":  iss.mint(t, map[string]any{"tid": "tenant-1"}),
		"malformed token": "not.a.jwt",
		"unknown issuer":  iss.mint(t, claims(map[string]any{"iss": "https://evil.example.com"})),
	} {
		_, err := v.Verify(ctx, s, tok)
		if got := TokenErrorReason(err); got != want || IssuerUnavailable(err) {
			t.Errorf("%s: reason %q (unavailable %v), err %v", want, got, IssuerUnavailable(err), err)
		}
	}

	// Discovery failing is the issuer's outage, not the token's fault.
	down := newFakeIssuer(t)
	tok := down.mint(t, claims(nil))
	down.srv.Close()
	s.Issuers[0].IssuerURL = down.srv.URL
	_, err := v.Verify(ctx, s, tok)
	if !IssuerUnavailable(err) || TokenErrorReason(err) != "issuer unavailable" {
		t.Fatalf("issuer down: %v", err)
	}
	if strings.Contains(TokenErrorReason(err), down.srv.URL) {
		t.Fatalf("the reason names the issuer's URL: %q", TokenErrorReason(err))
	}
}

// TestCloudTokenEmailLowercase: emails are lowercased rune by rune
// (strings.ToLower), as gateways do (gateway/src/cloud/jwt.rs lower_email),
// so the email hashes in a gateway's config match what it computes.
func TestCloudTokenEmailLowercase(t *testing.T) {
	iss := newFakeIssuer(t)
	s := testSnapshot(iss)
	v := NewCloudValidator(nil)
	for in, want := range map[string]string{
		"İstanbul@x.com":  "istanbul@x.com",
		"ΟΔΥΣΣΕΥΣ@x.com":  "οδυσσευσ@x.com", // no final ς
		"Dev@Example.COM": "dev@example.com",
	} {
		vt, err := v.Verify(context.Background(), s, iss.mint(t, map[string]any{"preferred_username": in, "tid": "tenant-1"}))
		if err != nil || vt.Email != want {
			t.Errorf("%s: %+v %v, want %s", in, vt, err, want)
		}
	}
}
