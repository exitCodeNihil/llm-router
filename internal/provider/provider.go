// Package provider builds upstream HTTP requests for each provider type.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// Transport is the shared upstream transport for all providers.
var Transport = &http.Transport{
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
}

var Client = &http.Client{Transport: Transport} // no timeout: streams can run long; caller uses ctx

// entraOnce lazily builds one DefaultAzureCredential shared by all Azure providers.
var (
	entraMu   sync.Mutex
	entraCred *azidentity.DefaultAzureCredential
	entraTok  struct {
		token   string
		expires time.Time
	}
)

const cognitiveScope = "https://cognitiveservices.azure.com/.default"

func entraToken(ctx context.Context) (string, error) {
	entraMu.Lock()
	defer entraMu.Unlock()
	if time.Until(entraTok.expires) > 2*time.Minute {
		return entraTok.token, nil
	}
	if entraCred == nil {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return "", fmt.Errorf("azure credential: %w", err)
		}
		entraCred = cred
	}
	tok, err := entraCred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{cognitiveScope}})
	if err != nil {
		return "", fmt.Errorf("entra token: %w", err)
	}
	entraTok.token, entraTok.expires = tok.Token, tok.ExpiresOn
	return tok.Token, nil
}

// ModelInfo describes one upstream model/deployment for discovery.
type ModelInfo struct {
	ID    string `json:"id"`              // what to put in upstream_name
	Model string `json:"model,omitempty"` // underlying model (azure deployments only)
	// Per-1M-token prices when the provider publishes them (OpenRouter does),
	// so the console can fill in custom pricing instead of asking for it.
	InputPer1M       *float64 `json:"input_per_1m,omitempty"`
	OutputPer1M      *float64 `json:"output_per_1m,omitempty"`
	CachedInputPer1M *float64 `json:"cached_input_per_1m,omitempty"`
	ContextTokens    int      `json:"context_tokens,omitempty"`
}

// OpenRouterBase is where OpenRouter lives; base_url may be left blank.
const OpenRouterBase = "https://openrouter.ai/api/v1"

// BaseURL is the provider's effective base URL, with the aggregator default
// filled in so an operator only has to paste a key.
func BaseURL(p *snapshot.Provider) string {
	if p.BaseURL == "" && p.Type == "openrouter" {
		return OpenRouterBase
	}
	return strings.TrimSuffix(p.BaseURL, "/")
}

// ListModels queries the provider for its available models: Azure's
// deployments API, or /models on any OpenAI-compatible server.
func ListModels(ctx context.Context, p *snapshot.Provider) ([]ModelInfo, error) {
	if p.Type == "gcp_vertex" {
		return listVertexModels(ctx, p)
	}
	var listURL string
	switch p.Type {
	case "azure":
		listURL = strings.TrimSuffix(p.BaseURL, "/") + "/openai/deployments?api-version=2023-03-15-preview"
	default:
		listURL = BaseURL(p) + "/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	if err := applyAuth(ctx, req, p, false); err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: Transport, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s returned %d: %s", listURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
			// OpenRouter: dollars per token as decimal strings.
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("unparseable model list: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		m := ModelInfo{ID: d.ID, Model: d.Model, ContextTokens: d.ContextLength}
		if m.Model == "" && p.Type == "openrouter" {
			m.Model = d.ID // keys the catalog row "openrouter/<slug>"
		}
		// Both must be present to make a price; a lone prompt rate is not one.
		if in, out := perMillion(d.Pricing.Prompt), perMillion(d.Pricing.Completion); in != nil && out != nil {
			m.InputPer1M, m.OutputPer1M, m.CachedInputPer1M = in, out, perMillion(d.Pricing.InputCacheRead)
		}
		out = append(out, m)
	}
	return out, nil
}

// perMillion turns a per-token dollar string ("0.0000003") into $/1M tokens.
// Negative means "dynamic pricing" on OpenRouter and is treated as unknown.
func perMillion(perToken string) *float64 {
	if perToken == "" {
		return nil
	}
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil || v < 0 {
		return nil
	}
	// Round away binary noise: 0.0000003 * 1e6 is 0.30000000000000004.
	v = math.Round(v*1e6*1e6) / 1e6
	return &v
}

// NewRequest builds the upstream request for a deployment: URL, auth headers,
// and the (already model-rewritten) body. Deployments with api_flavor
// "anthropic" (Claude on Azure AI Foundry) go to the Anthropic Messages
// surface regardless of path.
func NewRequest(ctx context.Context, d *snapshot.Deployment, path string, body []byte) (*http.Request, error) {
	p := d.Provider
	var upstreamURL string
	var err error
	anthropic := d.APIFlavor == "anthropic"
	switch {
	case p.Type == "gcp_vertex":
		if upstreamURL, body, err = vertexRequest(p, d, path, body, anthropic); err != nil {
			return nil, err
		}
	case anthropic:
		if path != "/chat/completions" && path != "/messages" {
			return nil, fmt.Errorf("%s is not supported by anthropic-flavor deployments", path)
		}
		base := strings.TrimSuffix(p.BaseURL, "/")
		if p.Type == "azure" {
			// https://{resource}.services.ai.azure.com/anthropic/v1/messages
			upstreamURL = base + "/anthropic/v1/messages"
		} else {
			// direct Anthropic API (api.anthropic.com) or any Anthropic-compatible server
			upstreamURL = strings.TrimSuffix(base, "/v1") + "/v1/messages"
		}
	case p.Type == "azure":
		apiVersion := p.APIVersion
		if apiVersion == "" {
			apiVersion = "2024-10-21"
		}
		// https://{resource}.openai.azure.com/openai/deployments/{deployment}{path}?api-version=...
		upstreamURL = strings.TrimSuffix(p.BaseURL, "/") +
			"/openai/deployments/" + url.PathEscape(d.UpstreamName) + path +
			"?api-version=" + url.QueryEscape(apiVersion)
	default: // openai_compatible / openrouter: vLLM, LM Studio, TGI, Ollama, OpenAI itself
		upstreamURL = BaseURL(p) + path
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	in := inboundFrom(ctx)
	if anthropic {
		// Vertex carries the version in the body instead, and rejects requests
		// that also pin it in the header.
		if p.Type != "gcp_vertex" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		// The client's beta list carries prompt caching, interleaved thinking and
		// context management; dropping it silently degrades those features.
		if in.AnthropicBeta != "" {
			req.Header.Set("anthropic-beta", in.AnthropicBeta)
		}
	}

	if err := applyAuth(ctx, req, p, anthropic); err != nil {
		return nil, err
	}
	return req, nil
}

// applyAuth sets the upstream credential headers for p. Shared by the proxy and
// by model discovery so a new auth mode only has to be taught once.
func applyAuth(ctx context.Context, req *http.Request, p *snapshot.Provider, anthropic bool) error {
	in := inboundFrom(ctx)
	switch p.AuthMode {
	case "entra":
		tok, err := entraToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case "gcp_adc", "gcp_sa":
		tok, err := gcpToken(p)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case "api_key": // Azure api-key header; the Anthropic surface reads x-api-key
		if anthropic {
			req.Header.Set("x-api-key", p.APIKey)
		} else {
			req.Header.Set("api-key", p.APIKey)
		}
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	case "oauth_passthrough":
		// The caller brings its own upstream credential — Claude Code sends the
		// subscription OAuth token it already holds — so forward it untouched and
		// store nothing here. Empty when the caller authenticated with the gateway
		// key itself, which must never travel upstream.
		if in.Authorization == "" {
			// Refuse rather than call the upstream unauthenticated: its own error
			// ("x-api-key header is required") gives no hint that the caller was
			// supposed to bring a credential. Hit from the console playground,
			// whose operator session has no upstream token to forward.
			return fmt.Errorf("%w: provider %q forwards the caller's credential — "+
				"send the upstream token in Authorization and the gateway key in X-Llmr-Key",
				ErrNoCallerCredential, p.Name)
		}
		req.Header.Set("Authorization", in.Authorization)
	case "none":
	}
	return nil
}
