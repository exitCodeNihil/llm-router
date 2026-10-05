package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/pricing"
	"github.com/exitcodenihil/llm-router/internal/provider"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

const maxBodyBytes = 10 << 20

type usageCounts struct {
	Prompt     int
	Completion int
	Cached     int
	// TTFTMs is set by the streaming paths; 0 for buffered responses.
	TTFTMs int
}

func (g *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	snap := g.Snapshots.Get()
	id := IdentityFrom(r.Context())
	start := time.Now()
	reqID := requestID()

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "could not read request body")
		return
	}
	if len(raw) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body exceeds 10MB")
		return
	}
	body, err := parseBody(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
		return
	}
	if body.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model field is required")
		return
	}
	alias, deployments := snap.Resolve(body.Model)
	if !id.ModelAllowed(body.Model, alias) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_allowed",
			"model "+body.Model+" is not available for this API key")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusNotFound, start, nil, "",
			telemetryDecision(snap, id), emitExtra{ErrorCode: "model_not_allowed"})
		return
	}
	if len(deployments) == 0 {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"model "+body.Model+" does not exist")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusNotFound, start, nil, "",
			telemetryDecision(snap, id), emitExtra{ErrorCode: "model_not_found"})
		return
	}

	injectUsage := body.Stream && !body.IncludeUsage

	// telemetry export decision computed once, now that identity is known
	dec := telemetryDecision(snap, id)

	resp, chosen, attempts, err := tryDeployments(r.Context(), deployments, r.URL.Path[len("/v1"):], func(d *snapshot.Deployment) ([]byte, error) {
		if d.APIFlavor == "anthropic" {
			return oaiToAnthBody(body, d.UpstreamName)
		}
		return body.upstreamBody(d.UpstreamName, injectUsage)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away
		}
		if errors.Is(err, provider.ErrNoCallerCredential) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_credential", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
		return
	}
	if resp == nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_unavailable",
			"all deployments for model "+body.Model+" failed")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusBadGateway, start, nil, "", dec,
			emitExtra{Attempts: attempts, ErrorCode: "upstream_unavailable"})
		return
	}
	defer resp.Body.Close()

	// Routing attribution, set before any body byte so it survives streaming.
	// Lets any caller correlate a response with its usage row and see which
	// deployment actually served it, including whether failover kicked in.
	setRoutingHeaders(w, reqID, chosen, attempts)

	capture := dec.CaptureContent
	var counts usageCounts
	var output string
	isSSE := body.Stream && resp.StatusCode == http.StatusOK
	switch {
	case chosen.APIFlavor == "anthropic" && isSSE:
		counts, output = anthStreamToOAI(w, resp, body.Model, reqID, capture, start)
	case chosen.APIFlavor == "anthropic" && resp.StatusCode == http.StatusOK:
		counts, output = anthRespToOAI(w, resp, body.Model, reqID, capture)
	case isSSE:
		counts, output = g.streamSSE(w, resp, injectUsage, capture, start)
	default:
		counts, output = copyJSON(w, resp, capture)
	}

	var input json.RawMessage
	if capture {
		input = body.fields["messages"]
	}
	e := g.emit(snap, id, chosen, body, reqID, counts, resp.StatusCode, start, input, output, dec,
		emitExtra{Attempts: attempts, ErrorCode: upstreamErrorCode(resp.StatusCode)})
	if g.Limits != nil {
		g.Limits.RecordUsage(id, counts.Prompt+counts.Completion, e.CostUSD)
	}
}

// telemetryDecision computes the export/capture verdict once per request from
// the identity's key/user/team ids and aggregated tags.
func telemetryDecision(snap *snapshot.Snapshot, id *auth.Identity) snapshot.Decision {
	var tags []string
	keyID, userID, teamID := "", "", ""
	if id.Key != nil {
		keyID = id.Key.ID
		tags = append(tags, id.Key.Tags...)
	}
	if id.User != nil {
		userID = id.User.ID
		tags = append(tags, id.User.Tags...)
	}
	if id.Team != nil {
		teamID = id.Team.ID
		tags = append(tags, id.Team.Tags...)
	}
	return snap.Telemetry.Evaluate(keyID, userID, teamID, tags)
}

// upstreamErrorCode labels a non-2xx answer from the provider. The gateway's own
// refusals carry more specific codes; this only covers what the upstream said.
func upstreamErrorCode(status int) string {
	switch {
	case status < 400:
		return ""
	case status == 429:
		return "upstream_rate_limited"
	case status >= 500:
		return "upstream_error"
	default:
		return "upstream_rejected"
	}
}

// setRoutingHeaders publishes the request id and the deployment that served
// the request. Attempts is 1 when the first-choice deployment answered, and
// higher when failover was used.
func setRoutingHeaders(w http.ResponseWriter, reqID string,
	chosen *snapshot.Deployment, attempts int) {
	h := w.Header()
	h.Set("X-Request-Id", reqID)
	if chosen == nil {
		return
	}
	if chosen.Provider != nil {
		h.Set("X-Llmr-Provider", chosen.Provider.Name)
	}
	h.Set("X-Llmr-Upstream", chosen.UpstreamName)
	h.Set("X-Llmr-Attempts", strconv.Itoa(attempts))
	// Browser clients on another origin can only read these when exposed.
	h.Set("Access-Control-Expose-Headers",
		"X-Request-Id, X-Llmr-Provider, X-Llmr-Upstream, X-Llmr-Attempts")
}

// tryDeployments tries each deployment in priority order, failing over on
// connect errors, 429, and 5xx. Retries happen only before any byte reaches
// the client. Returns (nil, nil, nil) when every deployment failed, and a
// non-nil error only for unrecoverable request-build failures or a dead client.
func tryDeployments(ctx context.Context, deployments []*snapshot.Deployment, upstreamPath string,
	buildBody func(*snapshot.Deployment) ([]byte, error)) (*http.Response, *snapshot.Deployment, int, error) {
	deployments = orderByHealth(deployments, time.Now())
	// Kept so a request that never reached any upstream can say why, instead of
	// reporting the generic "all deployments failed".
	var lastBuildErr error
	attempts := 0
	for i, d := range deployments {
		attempts++
		upBody, err := buildBody(d)
		if err != nil {
			return nil, nil, attempts, err
		}
		req, err := provider.NewRequest(ctx, d, upstreamPath, upBody)
		if err != nil {
			lastBuildErr = err
			slog.Warn("upstream request build failed", "deployment", d.UpstreamName, "err", err)
			continue
		}
		res, err := provider.Client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, attempts, ctx.Err()
			}
			upstreamHealth.fail(d.ID, 0, time.Now())
			slog.Warn("upstream connect failed", "provider", d.Provider.Name, "deployment", d.UpstreamName, "err", err)
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			upstreamHealth.fail(d.ID, retryAfter(res), time.Now())
			if i < len(deployments)-1 {
				res.Body.Close()
				slog.Warn("upstream returned retryable status; failing over",
					"provider", d.Provider.Name, "deployment", d.UpstreamName, "status", res.StatusCode)
				continue
			}
		} else {
			upstreamHealth.ok(d.ID)
		}
		return res, d, attempts, nil
	}
	return nil, nil, attempts, lastBuildErr
}

// orderByHealth keeps priority order but moves deployments in cooldown to the
// back, so they are only reached when everything healthier has failed —
// which is also how a recovered upstream gets probed again.
func orderByHealth(ds []*snapshot.Deployment, now time.Time) []*snapshot.Deployment {
	var healthy, cooling []*snapshot.Deployment
	for _, d := range ds {
		if upstreamHealth.cooling(d.ID, now) {
			cooling = append(cooling, d)
		} else {
			healthy = append(healthy, d)
		}
	}
	if len(cooling) == 0 {
		return ds
	}
	return append(healthy, cooling...)
}

// streamSSE copies the upstream SSE stream to the client, extracting token
// usage from the final usage chunk. If we injected include_usage ourselves,
// that chunk is dropped from the client copy.
func (g *Server) streamSSE(w http.ResponseWriter, resp *http.Response, dropUsageChunk, capture bool, ttftStart time.Time) (usageCounts, string) {
	var counts usageCounts
	var output strings.Builder
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxBodyBytes)
	for sc.Scan() {
		// First byte of actual content: TTFT is what a streaming client feels,
		// and total latency hides it behind however long the answer ran.
		if counts.TTFTMs == 0 {
			counts.TTFTMs = int(time.Since(ttftStart).Milliseconds())
		}
		line := sc.Bytes()
		forward := true
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && !bytes.Equal(data, []byte("[DONE]")) {
			if capture && output.Len() < 64<<10 && bytes.Contains(data, []byte(`"content"`)) {
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal(data, &chunk) == nil && len(chunk.Choices) > 0 {
					output.WriteString(chunk.Choices[0].Delta.Content)
				}
			}
			if bytes.Contains(data, []byte(`"usage"`)) {
				var chunk struct {
					Usage   *usageJSON        `json:"usage"`
					Choices []json.RawMessage `json:"choices"`
				}
				if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
					counts = chunk.Usage.counts()
					if dropUsageChunk && len(chunk.Choices) == 0 {
						forward = false
					}
				}
			}
		}
		if forward {
			w.Write(line)
			w.Write([]byte("\n"))
			if len(line) == 0 && flusher != nil {
				flusher.Flush() // blank line = end of SSE event
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("upstream stream ended abnormally", "err", err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return counts, output.String()
}

// copyJSON forwards a non-streaming response and parses its usage object.
func copyJSON(w http.ResponseWriter, resp *http.Response, capture bool) (usageCounts, string) {
	var counts usageCounts
	var output string
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		slog.Warn("upstream body read failed", "err", err)
	}
	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Usage   *usageJSON `json:"usage"`
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			if parsed.Usage != nil {
				counts = parsed.Usage.counts()
			}
			if capture && len(parsed.Choices) > 0 && parsed.Choices[0].Message.Content != nil {
				output = *parsed.Choices[0].Message.Content
			}
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	return counts, output
}

type usageJSON struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *usageJSON) counts() usageCounts {
	return usageCounts{Prompt: u.PromptTokens, Completion: u.CompletionTokens, Cached: u.PromptTokensDetails.CachedTokens}
}

// emitExtra carries how the request went, rather than widening an already long
// parameter list three more times.
type emitExtra struct {
	Attempts  int
	ErrorCode string
}

func (g *Server) emit(snap *snapshot.Snapshot, id *auth.Identity, d *snapshot.Deployment, body *reqBody,
	reqID string, counts usageCounts, status int, start time.Time, input json.RawMessage, output string,
	dec snapshot.Decision, extra emitExtra) usage.Event {
	e := usage.Event{
		Input:            input,
		Output:           output,
		Export:           dec.Export,
		CaptureContent:   dec.CaptureContent,
		RuleID:           dec.RuleID,
		TS:               start.UTC(),
		RequestID:        reqID,
		ModelName:        body.Model,
		PromptTokens:     counts.Prompt,
		CompletionTokens: counts.Completion,
		CachedTokens:     counts.Cached,
		LatencyMS:        int(time.Since(start).Milliseconds()),
		StatusCode:       status,
		Stream:           body.Stream,
		EdgeNodeID:       g.EdgeNodeID,
		TTFTMs:           counts.TTFTMs,
		Attempts:         extra.Attempts,
		ErrorCode:        extra.ErrorCode,
	}
	if id.Key != nil {
		e.APIKeyID = id.Key.ID
		e.Tags = append(e.Tags, id.Key.Tags...)
	}
	if id.User != nil {
		e.UserID = id.User.ID
		e.Tags = append(e.Tags, id.User.Tags...)
	}
	if id.Team != nil {
		e.TeamID = id.Team.ID
		e.Tags = append(e.Tags, id.Team.Tags...)
	}
	if d != nil {
		e.DeploymentID = d.ID
		e.ProviderID = d.Provider.ID
		e.CostUSD, e.Unpriced = pricing.Cost(snap, d, counts.Prompt, counts.Completion, counts.Cached)
		if e.Unpriced {
			// No real money changed hands, but the tokens still have a list value.
			e.NotionalCostUSD = pricing.Notional(snap, d, counts.Prompt, counts.Completion, counts.Cached)
		}
	}
	g.Usage.Write(e)
	return e
}
