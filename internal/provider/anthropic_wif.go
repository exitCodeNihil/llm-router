package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	// Renewal starts about halfway through a token's life, in the background, so
	// an outage of the identity provider or Anthropic shorter than that costs
	// nothing; and always this long before expiry at the latest.
	wifAdvisoryRefresh = 120 * time.Second
	// Too close to expiry to send: requests wait for a new token instead.
	wifMandatoryRefresh = 30 * time.Second
	// A refused exchange isn't retried on every request: a token with a jti claim
	// is single use, and the file has to rotate before another try can work.
	wifRetryAfter      = 5 * time.Second
	wifMaxRetryAfter   = 30 * time.Second
	wifExchangeTimeout = 15 * time.Second
	wifMaxAssertion    = 16 << 10 // Anthropic's limit
)

type wifToken struct {
	token     string
	expires   time.Time
	refreshAt time.Time
}

func newWIFToken(token string, lifetime time.Duration) *wifToken {
	now := time.Now()
	return &wifToken{token: token, expires: now.Add(lifetime), refreshAt: now.Add(wifRefreshAfter(lifetime))}
}

// wifRefreshAfter is half the lifetime, give or take a tenth, so pods that
// started together don't all renew in the same second.
func wifRefreshAfter(lifetime time.Duration) time.Duration {
	spread := int64(lifetime / 5)
	if spread <= 0 {
		return lifetime / 2
	}
	return lifetime/2 - lifetime/10 + time.Duration(rand.Int64N(spread))
}

// wifBackoff doubles from wifRetryAfter up to wifMaxRetryAfter, with jitter.
func wifBackoff(failures int) time.Duration {
	d := wifRetryAfter << min(max(failures-1, 0), 4)
	d = min(d, wifMaxRetryAfter)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

type wifState struct {
	current atomic.Pointer[wifToken]
	nextTry atomic.Int64 // unix nanos: no exchange before then

	refresh  sync.Mutex // one exchange at a time; guards the fields below
	lastErr  error
	failures int
	used     [sha256.Size]byte // the identity token behind current
}

// wifStates outlives config reloads: a reload builds new Provider values, and
// exchanging again for each one would burn single-use identity tokens.
var wifStates sync.Map // base URL + config -> *wifState

func wifStateFor(p *snapshot.Provider) *wifState {
	// Providers with the same settings share a token, since exchanging for each
	// would burn one identity token twice. The base URL is part of it: a repointed
	// provider must not receive a token minted for the old address.
	key := struct {
		base string
		cfg  snapshot.AnthropicWIF
	}{BaseURL(p), p.WIF}
	st, _ := wifStates.LoadOrStore(key, &wifState{})
	return st.(*wifState)
}

func anthropicWIFToken(ctx context.Context, p *snapshot.Provider) (string, error) {
	st := wifStateFor(p)
	now := time.Now()
	if cur := st.current.Load(); cur != nil && cur.expires.Sub(now) > wifMandatoryRefresh {
		if (!now.Before(cur.refreshAt) || cur.expires.Sub(now) <= wifAdvisoryRefresh) &&
			now.UnixNano() >= st.nextTry.Load() && st.refresh.TryLock() {
			go func() {
				defer st.refresh.Unlock()
				// Not the request's context: it must outlive the request.
				st.exchange(context.WithoutCancel(ctx), p)
			}()
		}
		return cur.token, nil
	}
	// Nothing usable: this request waits for an exchange.
	st.refresh.Lock()
	defer st.refresh.Unlock()
	return st.exchange(context.WithoutCancel(ctx), p)
}

// exchange gets a new token if one is due and allowed, and returns the token to
// send. The caller holds st.refresh.
func (st *wifState) exchange(ctx context.Context, p *snapshot.Provider) (string, error) {
	now := time.Now()
	cur := st.current.Load()
	usable := cur != nil && cur.expires.Sub(now) > wifMandatoryRefresh
	if usable && now.Before(cur.refreshAt) && cur.expires.Sub(now) > wifAdvisoryRefresh {
		return cur.token, nil // another exchange just finished
	}
	// Backing off: after a failure, or while waiting for the file to rotate. Once
	// the current token is too close to expiry, only a failure stops a try.
	if now.UnixNano() < st.nextTry.Load() && (usable || st.lastErr != nil) {
		if usable {
			return cur.token, nil
		}
		return "", st.lastErr
	}
	assertion, err := identityToken(ctx, p.WIF.IdentityTokenFile)
	sum := sha256.Sum256([]byte(assertion))
	// The file hasn't rotated yet. Sending it again would only be refused as a
	// replay, so wait while the current token lasts. Near expiry it is sent anyway,
	// since an issuer with check_jti off accepts it.
	if err == nil && usable && sum == st.used {
		st.nextTry.Store(now.Add(wifRetryAfter).UnixNano())
		return cur.token, nil
	}
	var next *wifToken
	if err == nil {
		next, err = exchangeWIF(ctx, p, assertion)
	}
	if err != nil {
		st.failures++
		st.lastErr = err
		st.nextTry.Store(now.Add(wifBackoff(st.failures)).UnixNano())
		slog.Warn("anthropic workload identity exchange failed", "provider", p.Name, "err", err)
		if usable {
			return cur.token, nil
		}
		return "", err
	}
	st.failures, st.lastErr, st.used = 0, nil, sum
	st.nextTry.Store(0)
	st.current.Store(next)
	return next.token, nil
}

func exchangeWIF(ctx context.Context, p *snapshot.Provider, assertion string) (*wifToken, error) {
	endpoint, err := wifEndpoint(BaseURL(p))
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
	return newWIFToken(out.AccessToken, time.Duration(out.ExpiresIn)*time.Second), nil
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

// identityToken is read or requested on every exchange, so rotated tokens get
// picked up. Where it comes from is per machine: the provider's path, a token
// requested from Kubernetes, or the variables Anthropic's SDKs read.
func identityToken(ctx context.Context, path string) (string, error) {
	if path == "" && os.Getenv("ANTHROPIC_IDENTITY_TOKEN_SOURCE") == "kubernetes" {
		token, err := kubernetesToken(ctx)
		if err == nil {
			return token, nil
		}
		// The file the kubelet keeps fresh still works while the API server is
		// unreachable, as long as it hasn't been exchanged yet.
		if fallback, ferr := fileToken(""); ferr == nil {
			// Once a minute at most: while the file waits to rotate this runs
			// every few seconds.
			if now := time.Now().UnixNano(); now-k8sFallbackWarned.Load() > int64(time.Minute) {
				k8sFallbackWarned.Store(now)
				slog.Warn("anthropic workload identity: kubernetes token request failed, using the token file", "err", err)
			}
			return fallback, nil
		}
		return "", err
	}
	return fileToken(path)
}

func fileToken(path string) (string, error) {
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
	return checkAssertion(token)
}

// checkAssertion sends nothing but a JWT: the path is admin-set and the contents
// go to the base URL, so this must not read arbitrary files out to a server.
func checkAssertion(token string) (string, error) {
	if len(token) > wifMaxAssertion || !isJWTShape(token) {
		return "", errors.New("anthropic workload identity: the identity token is not a JWT")
	}
	return token, nil
}

var k8sFallbackWarned atomic.Int64

// k8sServiceAccountDir holds the pod's own API credentials; a variable for tests.
var k8sServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// kubernetesToken asks the API server for a new service account token, bound to
// this pod, so every exchange presents a token no process has used. The chart
// grants the pod create on its own serviceaccounts/token and sets the LLMR_POD_*
// variables from the downward API.
func kubernetesToken(ctx context.Context) (string, error) {
	fail := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("anthropic workload identity: kubernetes token request: "+format, a...)
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	sa := os.Getenv("LLMR_POD_SERVICE_ACCOUNT")
	if host == "" || sa == "" {
		return fail("needs KUBERNETES_SERVICE_HOST and LLMR_POD_SERVICE_ACCOUNT")
	}
	if port == "" {
		port = "443"
	}
	ns := os.Getenv("LLMR_POD_NAMESPACE")
	if ns == "" {
		raw, err := os.ReadFile(filepath.Join(k8sServiceAccountDir, "namespace"))
		if err != nil {
			return fail("%v", err)
		}
		ns = strings.TrimSpace(string(raw))
	}
	bearer, err := os.ReadFile(filepath.Join(k8sServiceAccountDir, "token"))
	if err != nil {
		return fail("%v", err)
	}
	ca, err := os.ReadFile(filepath.Join(k8sServiceAccountDir, "ca.crt"))
	if err != nil {
		return fail("%v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fail("no certificate in ca.crt")
	}
	audience := os.Getenv("ANTHROPIC_IDENTITY_TOKEN_AUDIENCE")
	if audience == "" {
		audience = "https://api.anthropic.com"
	}
	lifetime := int64(3600) // the most an Anthropic issuer accepts by default
	if v, err := strconv.ParseInt(os.Getenv("ANTHROPIC_IDENTITY_TOKEN_EXPIRATION"), 10, 64); err == nil && v >= 600 {
		lifetime = v
	}
	spec := map[string]any{"audiences": []string{audience}, "expirationSeconds": lifetime}
	if name, uid := os.Getenv("LLMR_POD_NAME"), os.Getenv("LLMR_POD_UID"); name != "" && uid != "" {
		spec["boundObjectRef"] = map[string]string{"apiVersion": "v1", "kind": "Pod", "name": name, "uid": uid}
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest", "spec": spec})
	endpoint := "https://" + net.JoinHostPort(host, port) + "/api/v1/namespaces/" + url.PathEscape(ns) +
		"/serviceaccounts/" + url.PathEscape(sa) + "/token"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(bearer)))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return fail("%v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return fail("%v", err)
	}
	var out struct {
		Message string `json:"message"`
		Status  struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	json.Unmarshal(raw, &out)
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fail("%d: %s", res.StatusCode, out.Message)
	}
	return checkAssertion(strings.TrimSpace(out.Status.Token))
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
