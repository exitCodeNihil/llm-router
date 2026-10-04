package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// SSOConfig is the single OIDC provider configuration, stored in settings
// under key 'sso'. It covers Entra ID, Google, Okta, and any OIDC IdP.
type SSOConfig struct {
	IssuerURL    string `json:"issuer_url"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"` // e.g. https://router.example.com/auth/callback
	// AutoCreateUsers provisions a member account on first SSO login.
	AutoCreateUsers bool `json:"auto_create_users"`
}

type Auth struct {
	Store         *store.Store
	EncryptionKey string
	CookieDomain  string

	mu       sync.Mutex
	provider *oidc.Provider
	issuer   string // issuer the cached provider was built for
}

func (a *Auth) Register(mux *http.ServeMux) {
	a.registerPassword(mux)
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Domain: a.CookieDomain})
		// The marker lets the console drop a bootstrap token it holds in
		// local storage, which no server-side action can clear.
		http.Redirect(w, r, "/?signed_out=1", http.StatusFound)
	})
}

func (a *Auth) loadSSO(ctx context.Context) (*SSOConfig, error) {
	var raw []byte
	err := a.Store.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var cfg SSOConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (a *Auth) oidcProvider(ctx context.Context, cfg *SSOConfig) (*oidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil && a.issuer == cfg.IssuerURL {
		return a.provider, nil
	}
	p, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	a.provider, a.issuer = p, cfg.IssuerURL
	return p, nil
}

func (a *Auth) oauthConfig(cfg *SSOConfig, p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     p.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.loadSSO(r.Context())
	if err != nil {
		http.Error(w, "SSO is not configured; log in with the admin token", http.StatusNotFound)
		return
	}
	p, err := a.oidcProvider(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc discovery failed", "issuer", cfg.IssuerURL, "err", err)
		http.Error(w, "identity provider unreachable", http.StatusBadGateway)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: "llmr_oauth_state", Value: state, Path: "/auth",
		MaxAge: 300, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauthConfig(cfg, p).AuthCodeURL(state), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie("llmr_oauth_state")
	if err != nil || r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	cfg, err := a.loadSSO(r.Context())
	if err != nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	p, err := a.oidcProvider(r.Context(), cfg)
	if err != nil {
		http.Error(w, "identity provider unreachable", http.StatusBadGateway)
		return
	}
	oauth := a.oauthConfig(cfg, p)
	tok, err := oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idToken, err := p.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "invalid id token", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email             string `json:"email"`
		EmailVerified     *bool  `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "unreadable claims", http.StatusBadRequest)
		return
	}
	// An address the provider itself does not vouch for must not map to an
	// account: on IdPs with self-service signup it would be anyone's to claim.
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		http.Error(w, "the identity provider reports this email address as unverified", http.StatusForbidden)
		return
	}
	email := claims.Email
	if email == "" {
		email = claims.PreferredUsername // Entra ID often omits `email`
	}
	if email == "" {
		// many IdPs (Zitadel, Keycloak default config) only expose profile
		// claims via the UserInfo endpoint, not inside the ID token
		if info, err := p.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); err == nil {
			var uiClaims struct {
				Email             string `json:"email"`
				PreferredUsername string `json:"preferred_username"`
				Name              string `json:"name"`
			}
			if info.Claims(&uiClaims) == nil {
				email = uiClaims.Email
				if email == "" {
					email = uiClaims.PreferredUsername
				}
				if claims.Name == "" {
					claims.Name = uiClaims.Name
				}
			}
		}
	}
	if email == "" {
		http.Error(w, "identity provider returned no email for this account", http.StatusBadRequest)
		return
	}

	var userID string
	err = a.Store.Pool.QueryRow(r.Context(),
		`SELECT id FROM users WHERE email=$1 AND NOT disabled`, email).Scan(&userID)
	if err != nil {
		if !cfg.AutoCreateUsers {
			http.Error(w, "no account for "+email+"; ask an admin to invite you", http.StatusForbidden)
			return
		}
		id := uuid.New()
		if _, err := a.Store.Pool.Exec(r.Context(),
			`INSERT INTO users (id, email, name, role) VALUES ($1,$2,$3,'member')
			 ON CONFLICT (email) DO NOTHING`, id, email, claims.Name); err != nil {
			http.Error(w, "user provisioning failed", http.StatusInternalServerError)
			return
		}
		if err := a.Store.Pool.QueryRow(r.Context(),
			`SELECT id FROM users WHERE email=$1`, email).Scan(&userID); err != nil {
			http.Error(w, "user lookup failed", http.StatusInternalServerError)
			return
		}
	}

	setSessionCookie(w, r, a.EncryptionKey, a.CookieDomain, userID)
	http.Redirect(w, r, "/", http.StatusFound)
}
