package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"wrong tenant":    iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "evil"}),
		"wrong audience":  iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1", "aud": "api://other"}),
		"unknown user":    iss.mint(t, map[string]any{"preferred_username": "stranger@example.com", "tid": "tenant-1"}),
		"missing email":   iss.mint(t, map[string]any{"tid": "tenant-1"}),
		"expired":         iss.mint(t, map[string]any{"preferred_username": "dev@example.com", "tid": "tenant-1", "exp": time.Now().Add(-time.Hour).Unix()}),
		"garbage":         "not.a.jwt",
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
