package provider

// Google Cloud Vertex AI. Two surfaces live behind one provider type:
// Gemini speaks OpenAI chat-completions at .../endpoints/openapi, and Claude
// speaks the Anthropic Messages protocol at .../publishers/anthropic/models/
// <model>:rawPredict. Which one a request takes is the deployment's api_flavor,
// exactly as it is on Azure AI Foundry.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// vertexAnthropicVersion is what Vertex calls the Anthropic API version. It
// travels in the body, not the anthropic-version header.
const vertexAnthropicVersion = "vertex-2023-10-16"

const gcpScope = "https://www.googleapis.com/auth/cloud-platform"

type gcpCred struct {
	fingerprint string
	ts          oauth2.TokenSource
}

var (
	gcpMu    sync.Mutex
	gcpCreds = map[string]gcpCred{}
)

// gcpToken mints an access token for p, caching one token source per provider.
// oauth2's own source handles expiry and refresh; the fingerprint means a
// re-pasted service account key takes effect without a restart.
func gcpToken(p *snapshot.Provider) (string, error) {
	sum := sha256.Sum256([]byte(p.AuthMode + "\x00" + p.APIKey))
	fp := hex.EncodeToString(sum[:8])

	gcpMu.Lock()
	c, ok := gcpCreds[p.ID]
	if !ok || c.fingerprint != fp {
		ts, err := newGCPTokenSource(p)
		if err != nil {
			gcpMu.Unlock()
			return "", err
		}
		c = gcpCred{fingerprint: fp, ts: ts}
		gcpCreds[p.ID] = c
	}
	gcpMu.Unlock()

	tok, err := c.ts.Token()
	if err != nil {
		return "", fmt.Errorf("google token: %w", err)
	}
	return tok.AccessToken, nil
}

func newGCPTokenSource(p *snapshot.Provider) (oauth2.TokenSource, error) {
	// Background, never the request context: oauth2 keeps this context for every
	// later refresh, so a cancelled request would poison the source for good.
	// The client carries a timeout because ReuseTokenSource holds its lock while
	// refreshing — a stalled oauth2.googleapis.com would otherwise block every
	// request on this provider for as long as Google felt like it.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient,
		&http.Client{Transport: Transport, Timeout: 20 * time.Second})
	var creds *google.Credentials
	var err error
	if p.AuthMode == "gcp_sa" {
		if strings.TrimSpace(p.APIKey) == "" {
			return nil, fmt.Errorf("provider %q: no service account JSON stored", p.Name)
		}
		// Pinned to a real service account key: the JSON is operator-pasted, and
		// an unvalidated external_account config would happily point token
		// minting at somebody else's endpoint.
		creds, err = google.CredentialsFromJSONWithType(ctx, []byte(p.APIKey), google.ServiceAccount, gcpScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, gcpScope)
	}
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	return creds.TokenSource, nil
}

// vertexLocationRe is what may become part of a hostname. Anything else would
// send the bearer token — cloud-platform scope — to whatever host it spells.
var vertexLocationRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidateVertex rejects a gcp_vertex provider the gateway could never route
// through, so the mistake surfaces at save time and not as a 500 per request.
func ValidateVertex(authMode, project, location string) error {
	if authMode != "gcp_adc" && authMode != "gcp_sa" {
		return fmt.Errorf("auth_mode %q is not valid for gcp_vertex (use gcp_adc or gcp_sa)", authMode)
	}
	if project == "" {
		return fmt.Errorf("config.project is required for gcp_vertex")
	}
	if location != "" && !vertexLocationRe.MatchString(location) {
		return fmt.Errorf("config.location %q is not a valid region", location)
	}
	return nil
}

func vertexLocation(p *snapshot.Provider) string {
	if p.Location != "" {
		return p.Location
	}
	return "global"
}

// publicVertexHost matches Google's own regional hosts. Those are derived from
// the location; a stored copy would go stale the moment the location changed,
// and Vertex rejects a path whose location does not match the host.
var publicVertexHost = regexp.MustCompile(`^https://([a-z0-9-]+-)?aiplatform\.googleapis\.com/?$`)

// vertexBase is the regional API host. base_url is honoured only for a private
// endpoint; the public host always follows the location.
func vertexBase(p *snapshot.Provider) string {
	if p.BaseURL != "" && !publicVertexHost.MatchString(p.BaseURL) {
		return strings.TrimSuffix(p.BaseURL, "/")
	}
	if loc := vertexLocation(p); loc != "global" {
		return "https://" + loc + "-aiplatform.googleapis.com"
	}
	return "https://aiplatform.googleapis.com"
}

func vertexParent(p *snapshot.Provider) string {
	return vertexBase(p) + "/v1/projects/" + url.PathEscape(p.Project) +
		"/locations/" + url.PathEscape(vertexLocation(p))
}

// vertexModelSegment escapes an upstream model id for the URL. Claude ids carry
// a version suffix ("claude-sonnet-4-5@20250929"); url.PathEscape leaves "@"
// alone and Vertex accepts either form, so encode it for the one that is
// unambiguous in every proxy on the way.
func vertexModelSegment(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), "@", "%40")
}

// vertexRequest returns the upstream URL and body for a Vertex deployment.
// Anthropic-flavour requests are reshaped: Vertex takes the model in the URL
// and rejects it in the body, and wants the API version there instead.
func vertexRequest(p *snapshot.Provider, d *snapshot.Deployment, path string, body []byte, anthropic bool) (string, []byte, error) {
	if p.Project == "" {
		return "", nil, fmt.Errorf("provider %q: gcp project is not set", p.Name)
	}
	// Both surfaces are chat-only: the OpenAI-compatible endpoint answers
	// /embeddings with FAILED_PRECONDITION, and rawPredict is Messages.
	if path != "/chat/completions" && path != "/messages" {
		return "", nil, fmt.Errorf("%s is not supported on vertex deployments", path)
	}
	if !anthropic {
		// Gemini's OpenAI-compatible surface; upstream_name carries the publisher
		// prefix ("google/gemini-2.5-flash") the endpoint expects in the body.
		return vertexParent(p) + "/endpoints/openapi" + path, body, nil
	}
	out, stream, err := vertexAnthBody(body)
	if err != nil {
		return "", nil, err
	}
	verb := ":rawPredict"
	if stream {
		verb = ":streamRawPredict"
	}
	return vertexParent(p) + "/publishers/anthropic/models/" + vertexModelSegment(d.UpstreamName) + verb, out, nil
}

func vertexAnthBody(body []byte) ([]byte, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, fmt.Errorf("invalid JSON body: %w", err)
	}
	delete(m, "model")
	if _, ok := m["anthropic_version"]; !ok {
		m["anthropic_version"] = json.RawMessage(`"` + vertexAnthropicVersion + `"`)
	}
	var stream bool
	if s, ok := m["stream"]; ok {
		_ = json.Unmarshal(s, &stream)
	}
	out, err := json.Marshal(m)
	return out, stream, err
}

// vertexPublishers are the Model Garden publishers whose catalogue the listing
// API actually returns. Qwen, GLM, Llama, DeepSeek and the rest answer with an
// empty list, so those MaaS models are typed in by hand as "publisher/model".
var vertexPublishers = []string{"google", "anthropic"}

// listVertexModels returns the publisher catalogues for discovery. Vertex has
// no project-scoped model list; what it has is one catalogue per publisher.
func listVertexModels(ctx context.Context, p *snapshot.Provider) ([]ModelInfo, error) {
	if p.Project == "" {
		return nil, fmt.Errorf("provider %q: gcp project is not set", p.Name)
	}
	var out []ModelInfo
	var lastErr error
	for _, pub := range vertexPublishers {
		models, err := listVertexPublisher(ctx, p, pub)
		if err != nil {
			// One publisher's outage should not hide the others' catalogue.
			slog.Warn("vertex model listing failed", "provider", p.Name, "publisher", pub, "err", err)
			lastErr = err
			continue
		}
		out = append(out, models...)
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

func listVertexPublisher(ctx context.Context, p *snapshot.Provider, publisher string) ([]ModelInfo, error) {
	listURL := vertexBase(p) + "/v1beta1/publishers/" + publisher + "/models?pageSize=200"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	// Without a quota project this endpoint answers 200 with an empty list
	// rather than an error, which reads exactly like "no models available".
	req.Header.Set("x-goog-user-project", p.Project)
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
		PublisherModels []struct {
			Name      string `json:"name"` // publishers/google/models/gemini-2.5-flash
			VersionID string `json:"versionId"`
		} `json:"publisherModels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("unparseable model list: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.PublisherModels))
	for _, m := range parsed.PublisherModels {
		id := m.Name[strings.LastIndex(m.Name, "/")+1:]
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{ID: vertexUpstreamID(publisher, id, m.VersionID), Model: id})
	}
	return out, nil
}

// vertexUpstreamID is what goes in upstream_name. Claude is addressed by its
// own rawPredict path, so the id is bare and carries the version suffix Vertex
// requires ("claude-sonnet-4-5@20250929"; "default" means the id is the whole
// name). Everything else goes through the OpenAI-compatible endpoint, which
// wants the publisher prefix in the body ("google/gemini-2.5-flash").
func vertexUpstreamID(publisher, id, version string) string {
	if publisher == "anthropic" {
		if version != "" && version != "default" {
			return id + "@" + version
		}
		return id
	}
	return publisher + "/" + id
}
