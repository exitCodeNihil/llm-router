package console

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// fakeIdP is just enough of an OIDC provider: discovery, keys, and a token
// endpoint that enforces PKCE and single-use codes, as real ones do.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	email string

	mu    sync.Mutex
	codes map[string]*grant
}

type grant struct {
	challenge, nonce string
	used             bool
}

func newFakeIdP(t *testing.T, email string) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: key, email: email, codes: map[string]*grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		g := p.codes[r.Form.Get("code")]
		ok := g != nil && !g.used
		if ok {
			g.used = true
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			ok = base64.RawURLEncoding.EncodeToString(sum[:]) == g.challenge
		}
		p.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.idToken(t, g.nonce)})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) idToken(t *testing.T, nonce string) string {
	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now()
	input := b64(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + b64(map[string]any{
		"iss": p.srv.URL, "sub": "sub-1", "aud": "cid", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": nonce, "email": p.email, "email_verified": true,
	})
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// authorize is the user approving the sign-in at the IdP: it issues a code
// bound to the login's PKCE challenge and to nonce (normally the login's).
func (p *fakeIdP) authorize(authURL *url.URL, nonce string) string {
	code := uuid.NewString()
	p.mu.Lock()
	p.codes[code] = &grant{challenge: authURL.Query().Get("code_challenge"), nonce: nonce}
	p.mu.Unlock()
	return code
}

// TestOIDCFlow: state, nonce and PKCE each refuse a callback they do not
// match, and the flow cookies are spent on the first callback.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/console
func TestOIDCFlow(t *testing.T) {
	dbURL := os.Getenv("LLMR_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var oldSSO []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&oldSSO)
	defer func() {
		if oldSSO == nil {
			st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='sso'`)
		} else {
			st.Pool.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='sso'`, oldSSO)
		}
	}()

	userID := uuid.New()
	email := "oidc-" + userID.String()[:8] + "@example.com"
	if _, err := st.Pool.Exec(ctx, `INSERT INTO users (id, email, name, role) VALUES ($1,$2,'o','member')`, userID, email); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)

	idp := newFakeIdP(t, email)
	const key = "test-encryption-key-0123456789"
	if err := SaveSSO(ctx, st.Pool, key, &SSOConfig{IssuerURL: idp.srv.URL, ClientID: "cid", ClientSecret: "cs",
		RedirectURL: "https://router.test/auth/callback"}); err != nil {
		t.Fatal(err)
	}
	a := &Auth{Store: st, EncryptionKey: key}
	mux := http.NewServeMux()
	a.Register(mux)

	type login struct {
		authURL *url.URL
		cookies map[string]*http.Cookie
	}
	start := func() login {
		t.Helper()
		req := httptest.NewRequest("GET", "/auth/login", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("login: %d %s", w.Code, w.Body.String())
		}
		u, _ := url.Parse(w.Header().Get("Location"))
		q := u.Query()
		if q.Get("nonce") == "" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorization request lacks nonce or PKCE: %s", u)
		}
		l := login{authURL: u, cookies: map[string]*http.Cookie{}}
		for _, c := range w.Result().Cookies() {
			if !c.HttpOnly || !c.Secure || c.Path != "/auth" {
				t.Errorf("flow cookie %s: HttpOnly %v Secure %v Path %q", c.Name, c.HttpOnly, c.Secure, c.Path)
			}
			l.cookies[c.Name] = c
		}
		if len(l.cookies) != 3 {
			t.Fatalf("flow cookies: %v", l.cookies)
		}
		if l.cookies[nonceCookie].Value != q.Get("nonce") {
			t.Fatal("nonce cookie does not match the request")
		}
		return l
	}
	callback := func(l login, state, code string, skip ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", "/auth/callback?"+url.Values{"state": {state}, "code": {code}}.Encode(), nil)
	cookies:
		for name, c := range l.cookies {
			for _, s := range skip {
				if s == name {
					continue cookies
				}
			}
			req.AddCookie(&http.Cookie{Name: name, Value: c.Value})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	cleared := func(w *httptest.ResponseRecorder) {
		t.Helper()
		gone := map[string]bool{}
		for _, c := range w.Result().Cookies() {
			if c.MaxAge < 0 && c.Path == "/auth" {
				gone[c.Name] = true
			}
		}
		if !gone[stateCookie] || !gone[nonceCookie] || !gone[verifierCookie] {
			t.Errorf("flow cookies not all cleared (status %d): %v", w.Code, gone)
		}
	}
	session := func(w *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie && c.Value != "" {
				return c
			}
		}
		return nil
	}

	// Success.
	l := start()
	state := l.authURL.Query().Get("state")
	code := idp.authorize(l.authURL, l.authURL.Query().Get("nonce"))
	w := callback(l, state, code)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	cleared(w)
	if s := session(w); s == nil {
		t.Fatal("no session after a good callback")
	} else if got, err := VerifySession(key, s.Value); err != nil || got.UserID != userID.String() || got.SID == "" {
		t.Fatalf("session = %+v, %v", got, err)
	}

	// Replayed: the browser no longer holds the flow cookies; an attacker who
	// kept them meets a spent code.
	if w := callback(login{}, state, code); w.Code != http.StatusBadRequest {
		t.Errorf("replay without cookies: %d", w.Code)
	}
	if w := callback(l, state, code); w.Code != http.StatusBadGateway || session(w) != nil {
		t.Errorf("replay with the old cookies: %d", w.Code)
	}

	// Wrong state.
	l = start()
	w = callback(l, "not-"+l.authURL.Query().Get("state"), idp.authorize(l.authURL, l.authURL.Query().Get("nonce")))
	if w.Code != http.StatusBadRequest || session(w) != nil {
		t.Errorf("wrong state: %d", w.Code)
	}
	cleared(w)

	// An ID token minted for another sign-in.
	l = start()
	w = callback(l, l.authURL.Query().Get("state"), idp.authorize(l.authURL, "someone-elses-nonce"))
	if w.Code != http.StatusUnauthorized || session(w) != nil {
		t.Errorf("wrong nonce: %d %s", w.Code, w.Body.String())
	}
	cleared(w)

	// No verifier cookie: refused before the code is spent.
	l = start()
	w = callback(l, l.authURL.Query().Get("state"), idp.authorize(l.authURL, l.authURL.Query().Get("nonce")), verifierCookie)
	if w.Code != http.StatusBadRequest || session(w) != nil {
		t.Errorf("missing verifier: %d", w.Code)
	}
	cleared(w)

	// A verifier that does not match the challenge: the IdP refuses the code.
	l = start()
	l.cookies[verifierCookie] = &http.Cookie{Name: verifierCookie, Value: "a-verifier-of-another-sign-in-0123456789abcdef"}
	w = callback(l, l.authURL.Query().Get("state"), idp.authorize(l.authURL, l.authURL.Query().Get("nonce")))
	if w.Code != http.StatusBadGateway || session(w) != nil {
		t.Errorf("wrong PKCE verifier: %d", w.Code)
	}
	cleared(w)
}
