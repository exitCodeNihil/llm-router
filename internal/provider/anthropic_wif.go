package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// Anthropic workload identity federation: this process presents an identity token
// from its own platform (a Kubernetes projected token, say) and gets a short-lived
// Anthropic token back, so no static key exists to leak. platform.claude.com/docs/
// en/manage-claude/workload-identity-federation. gateway/src/wif.rs is the same
// logic for the Rust gateway.
const (
	// The schedule Anthropic's SDKs use: try a new token this long before expiry
	// but keep serving the old one if that fails, then fail requests once it is
	// too close to expiry to send.
	wifAdvisoryRefresh  = 120 * time.Second
	wifMandatoryRefresh = 30 * time.Second
	// A refused exchange isn't retried on every request: a token with a jti claim
	// is single use, and the file has to rotate before another try can work.
	wifRetryAfter      = 5 * time.Second
	wifExchangeTimeout = 15 * time.Second
	wifMaxAssertion    = 16 << 10 // Anthropic's limit
)

type wifToken struct {
	token   string
	expires time.Time
}

type wifState struct {
	current atomic.Pointer[wifToken]

	refresh sync.Mutex // one exchange at a time; guards the two fields below
	retryAt time.Time
	lastErr error
}

// wifStates outlives config reloads: a reload builds new Provider values, and
// exchanging again for each one would burn single-use identity tokens.
var wifStates sync.Map // providerID + config -> *wifState

func wifStateFor(p *snapshot.Provider) *wifState {
	// The base URL is part of it: a repointed provider must not receive a token
	// minted for the old address.
	key := struct {
		id, base string
		cfg      snapshot.AnthropicWIF
	}{p.ID, p.BaseURL, p.WIF}
	st, _ := wifStates.LoadOrStore(key, &wifState{})
	return st.(*wifState)
}

func anthropicWIFToken(ctx context.Context, p *snapshot.Provider) (string, error) {
	st := wifStateFor(p)
	left := func(t *wifToken) time.Duration {
		if t == nil {
			return 0
		}
		return time.Until(t.expires)
	}
	cur := st.current.Load()
	if left(cur) > wifAdvisoryRefresh {
		return cur.token, nil
	}
	if left(cur) > wifMandatoryRefresh {
		if !st.refresh.TryLock() {
			return cur.token, nil // another request is already refreshing it
		}
	} else {
		st.refresh.Lock()
	}
	defer st.refresh.Unlock()

	cur = st.current.Load() // it may have been refreshed while this one waited
	if left(cur) > wifAdvisoryRefresh {
		return cur.token, nil
	}
	usable := left(cur) > wifMandatoryRefresh
	if time.Now().Before(st.retryAt) {
		if usable {
			return cur.token, nil
		}
		return "", st.lastErr
	}
	// Not the request's context: a client hanging up must not waste an exchange
	// the next request is waiting for.
	next, err := exchangeWIF(context.WithoutCancel(ctx), p)
	if err != nil {
		st.retryAt, st.lastErr = time.Now().Add(wifRetryAfter), err
		slog.Warn("anthropic workload identity exchange failed", "provider", p.Name, "err", err)
		if usable {
			return cur.token, nil
		}
		return "", err
	}
	st.current.Store(next)
	return next.token, nil
}

func exchangeWIF(ctx context.Context, p *snapshot.Provider) (*wifToken, error) {
	endpoint, err := wifEndpoint(p.BaseURL)
	if err != nil {
		return nil, err
	}
	assertion, err := identityToken(p.WIF.IdentityTokenFile)
	if err != nil {
		return nil, err
	}
	grant := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:jwt-bearer",
		"assertion":          assertion,
		"federation_rule_id": p.WIF.FederationRuleID,
		"organization_id":    p.WIF.OrganizationID,
		"service_account_id": p.WIF.ServiceAccountID,
	}
	if p.WIF.WorkspaceID != "" {
		grant["workspace_id"] = p.WIF.WorkspaceID
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, wifExchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic workload identity: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("anthropic workload identity: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &e)
		hint := ""
		if res.StatusCode == http.StatusUnauthorized {
			hint = " (the reason is in the Claude Console: Settings, Workload identity, authentication history)"
		}
		return nil, fmt.Errorf("anthropic workload identity: token exchange returned %d: %s%s", res.StatusCode, e.Error.Message, hint)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return nil, errors.New("anthropic workload identity: token exchange returned no usable token")
	}
	return &wifToken{token: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}, nil
}

// wifEndpoint refuses plain HTTP to anything but this machine: the identity token
// is a credential until it expires.
func wifEndpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("anthropic workload identity: bad base URL %q", base)
	}
	h := u.Hostname()
	if u.Scheme != "https" && h != "localhost" && h != "127.0.0.1" && h != "::1" {
		return "", errors.New("anthropic workload identity: the base URL must use https")
	}
	return u.String() + "/v1/oauth/token", nil
}

// identityToken reads the token on every exchange, so rotated tokens get picked
// up. Where it comes from is per machine: the provider's path, or the environment
// variables Anthropic's SDKs read.
func identityToken(path string) (string, error) {
	if path == "" {
		path = os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE")
	}
	token := os.Getenv("ANTHROPIC_IDENTITY_TOKEN")
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("anthropic workload identity: %w", err)
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, wifMaxAssertion+1))
		if err != nil {
			return "", fmt.Errorf("anthropic workload identity: %w", err)
		}
		token = string(raw)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("anthropic workload identity: no identity token: set the provider's token file, " +
			"ANTHROPIC_IDENTITY_TOKEN_FILE or ANTHROPIC_IDENTITY_TOKEN")
	}
	// The path is admin-set and the contents go to the base URL, so never send
	// anything but a JWT: this must not read arbitrary files out to a server.
	if len(token) > wifMaxAssertion || !isJWTShape(token) {
		return "", errors.New("anthropic workload identity: the identity token is not a JWT")
	}
	return token, nil
}

func isJWTShape(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}

// ValidateAnthropicWIF checks a provider's federation settings when it is saved.
func ValidateAnthropicWIF(w snapshot.AnthropicWIF) error {
	switch {
	case !strings.HasPrefix(w.FederationRuleID, "fdrl_"):
		return errors.New("federation_rule_id must look like fdrl_...")
	case w.OrganizationID == "":
		return errors.New("organization_id is required")
	case !strings.HasPrefix(w.ServiceAccountID, "svac_"):
		return errors.New("service_account_id must look like svac_...")
	case w.WorkspaceID != "" && !strings.HasPrefix(w.WorkspaceID, "wrkspc_"):
		return errors.New("workspace_id must look like wrkspc_...")
	case w.IdentityTokenFile != "" && !strings.HasPrefix(w.IdentityTokenFile, "/"):
		return errors.New("identity_token_file must be an absolute path")
	}
	return nil
}
