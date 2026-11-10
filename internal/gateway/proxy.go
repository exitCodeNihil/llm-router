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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// Parts of Prompt that cost more than plain input, and the request options
	// that raise every rate. Only Anthropic upstreams report them.
	Write5m, Write1h  int
	USInference, Fast bool
	// Geo and Speed are inference_geo and speed as the upstream reported them.
	Geo, Speed string
	// TTFTMs is set by the streaming paths; 0 for buffered responses.
	TTFTMs int
}

func (g *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	snap := g.Snapshots.Get()
	id := IdentityFrom(r.Context())
	start := time.Now()
	reqID := requestID()
	tr := traceFrom(r.Context())

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
			telemetryDecision(snap, id, reqID, tr), emitExtra{Trace: tr, ErrorCode: "model_not_allowed"})
		return
	}
	if len(deployments) == 0 {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"model "+body.Model+" does not exist")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusNotFound, start, nil, "",
			telemetryDecision(snap, id, reqID, tr), emitExtra{Trace: tr, ErrorCode: "model_not_found"})
		return
	}

	injectUsage := body.Stream && !body.IncludeUsage

	// telemetry export decision computed once, now that identity is known
	dec := telemetryDecision(snap, id, reqID, tr)

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
		if code := callerErrorCode(err); code != "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", code, err.Error())
			g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusBadRequest, start, nil, "", dec,
				emitExtra{Trace: tr, Attempts: attempts, ErrorCode: code})
			return
		}
		// Recorded too, or a provider that can't be reached (a refused token
		// exchange, say) would fail requests without a trace in usage.
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusInternalServerError, start, nil, "", dec,
			emitExtra{Trace: tr, Attempts: attempts, ErrorCode: "internal_error"})
		return
	}
	if resp == nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_unavailable",
			"all deployments for model "+body.Model+" failed")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusBadGateway, start, nil, "", dec,
			emitExtra{Trace: tr, Attempts: attempts, ErrorCode: "upstream_unavailable"})
		return
	}
	defer resp.Body.Close()

	// Routing attribution, set before any body byte so it survives streaming.
	// Lets any caller correlate a response with its usage row and see which
	// deployment actually served it, including whether failover kicked in.
	setRoutingHeaders(w, reqID, chosen, attempts)
	forwardRetryAfter(w, resp)
	forwardRateLimits(w, resp)

	capture := contentCap(snap, id, dec)
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

	input, system, output, cut := capContent(capture, body.fields["messages"], nil, output)
	e := g.emit(snap, id, chosen, body, reqID, counts, resp.StatusCode, start, input, output, dec,
		emitExtra{Trace: tr, Attempts: attempts, ErrorCode: upstreamErrorCode(resp.StatusCode),
			System: system, Truncated: cut})
	if g.Limits != nil {
		g.Limits.RecordUsage(id, counts.Prompt+counts.Completion, e.CostUSD)
	}
}

// callerErrorCode labels a request-build failure that is the caller's mistake,
// so a 400 and not a 500. Empty for anything else.
func callerErrorCode(err error) string {
	switch {
	case errors.Is(err, provider.ErrNoCallerCredential):
		return "missing_credential"
	case errors.Is(err, provider.ErrUnsupportedRoute):
		return "unsupported_route"
	}
	return ""
}

// telemetryDecision computes the export/capture verdict once per request from
// the identity's key/user/team ids and aggregated tags. The caller's
// traceparent doesn't take part: its sampled flag would let a caller opt out.
func telemetryDecision(snap *snapshot.Snapshot, id *auth.Identity, reqID string, tr traceCtx) snapshot.Decision {
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
	return snap.Telemetry.Evaluate(keyID, userID, teamID, tags, reqID)
}

// contentCap is how many bytes of content to capture: none unless the policy
// captures it and a destination that would take this request wants it. The
// control plane's own gateway serves the control-plane group.
func contentCap(snap *snapshot.Snapshot, id *auth.Identity, dec snapshot.Decision) int {
	if !dec.CaptureContent {
		return 0
	}
	team := ""
	if id.Team != nil {
		team = id.Team.ID
	}
	return snap.ContentWanted(team, snapshot.ControlPlaneGroup)
}

// capContent bounds captured content to limit bytes per part: the output is
// cut, a larger system prompt has its text cut, and the input keeps its newest
// messages that fit, or the newest alone with its text cut when not even that
// fits. Destinations cut further to their own caps; this keeps a queued event
// from holding a 10 MB prompt. cut reports anything left out. The Rust
// gateway's capture_request follows the same rule.
func capContent(limit int, input, system json.RawMessage, output string) (json.RawMessage, json.RawMessage, string, bool) {
	if limit <= 0 {
		return nil, nil, "", false
	}
	cut := false
	if len(output) > limit {
		output, cut = truncateUTF8(output, limit), true
	}
	if len(system) > limit {
		system, cut = cutJSON(system, limit), true
	}
	if len(input) > limit {
		var msgs []json.RawMessage
		json.Unmarshal(input, &msgs)
		n, i := 2, len(msgs)
		for i > 0 && n+len(msgs[i-1])+1 <= limit {
			n += len(msgs[i-1]) + 1
			i--
		}
		input, cut = nil, true
		switch {
		case i < len(msgs):
			input, _ = json.Marshal(msgs[i:])
		case len(msgs) > 0: // not even the newest fits: keep it, its text cut
			if m := cutJSON(msgs[len(msgs)-1], limit-2); m != nil {
				input = append(append(json.RawMessage("["), m...), ']')
			}
		}
	}
	return input, system, output, cut
}

// cutJSON fits a JSON value in limit bytes by shortening its strings, the
// longest first, leaving "role" and "type" alone; nil when even empty strings
// don't fit. Only an oversized part takes this path: it is re-encoded.
func cutJSON(raw json.RawMessage, limit int) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	type ref struct {
		s   string
		set func(string)
	}
	var refs []ref
	var walk func(v any, set func(string))
	walk = func(v any, set func(string)) {
		switch x := v.(type) {
		case string:
			refs = append(refs, ref{x, set})
		case []any:
			for i := range x {
				walk(x[i], func(s string) { x[i] = s })
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if k != "role" && k != "type" {
					walk(x[k], func(s string) { x[k] = s })
				}
			}
		}
	}
	walk(v, func(s string) { v = s })
	out, err := marshalNoEscape(v)
	if err != nil {
		return nil
	}
	excess := len(out) - limit
	sort.SliceStable(refs, func(i, j int) bool { return len(refs[i].s) > len(refs[j].s) })
	// Every byte of text takes at least a byte encoded, so cutting n bytes of
	// text saves at least n.
	for _, r := range refs {
		if excess <= 0 {
			break
		}
		c := truncateUTF8(r.s, max(len(r.s)-excess, 0))
		r.set(c)
		excess -= len(r.s) - len(c)
	}
	if out, err = marshalNoEscape(v); err != nil || len(out) > limit {
		return nil
	}
	return out
}

// marshalNoEscape is json.Marshal without HTML escaping, which would grow the text.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func truncateUTF8(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
		"X-Request-Id, X-Llmr-Trace-Id, X-Llmr-Provider, X-Llmr-Upstream, X-Llmr-Attempts")
}

// defaultUpstreamTimeout bounds the wait for an upstream's response headers
// when a deployment sets no timeout_ms of its own.
const defaultUpstreamTimeout = 10 * time.Minute

// tryDeployments tries each deployment in priority order, failing over on
// connect errors, timeouts, 429, and 5xx. Retries happen only before any byte
// reaches the client. Returns (nil, nil, nil) when every deployment failed, and
// a non-nil error only for unrecoverable request-build failures or a dead client.
func tryDeployments(ctx context.Context, deployments []*snapshot.Deployment, upstreamPath string,
	buildBody func(*snapshot.Deployment) ([]byte, error)) (*http.Response, *snapshot.Deployment, int, error) {
	deployments = orderByHealth(deployments, time.Now())
	// Kept so a request that never reached any upstream can say why, instead of
	// reporting the generic "all deployments failed".
	var lastBuildErr error
	reached := false
	attempts := 0
	for i, d := range deployments {
		attempts++
		// Before the body: an /embeddings body isn't a chat request, so translating
		// it could fail and hide the actual reason.
		if err := provider.CheckRoute(d, upstreamPath); err != nil {
			lastBuildErr = err
			continue
		}
		upBody, err := buildBody(d)
		if err != nil {
			return nil, nil, attempts, err
		}
		// The timeout covers the wait for response headers only: a long stream
		// that has started is not cut off.
		timeout := defaultUpstreamTimeout
		if d.TimeoutMS != nil {
			timeout = time.Duration(*d.TimeoutMS) * time.Millisecond
		}
		actx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(timeout, cancel)
		req, err := provider.NewRequest(actx, d, upstreamPath, upBody)
		if err != nil {
			timer.Stop()
			cancel()
			lastBuildErr = err
			slog.Warn("upstream request build failed", "deployment", d.UpstreamName, "err", err)
			continue
		}
		reached = true
		res, err := provider.Client.Do(req)
		if timedOut := !timer.Stop(); err != nil || timedOut {
			cancel()
			if res != nil {
				res.Body.Close()
			}
			if ctx.Err() != nil {
				return nil, nil, attempts, ctx.Err()
			}
			upstreamHealth.fail(d.ID, 0, time.Now())
			slog.Warn("upstream failed", "provider", d.Provider.Name, "deployment", d.UpstreamName,
				"timed_out", timedOut, "err", err)
			continue
		}
		res.Body = cancelOnClose{res.Body, cancel}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			// A pass-through 429 is the caller's own plan quota, not a sick backend:
			// cooling the shared row would mark it down for every other subscriber.
			if res.StatusCode >= 500 || d.Provider.AuthMode != "oauth_passthrough" {
				upstreamHealth.fail(d.ID, retryAfter(res), time.Now())
			}
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
	if reached {
		return nil, nil, attempts, nil
	}
	return nil, nil, attempts, lastBuildErr
}

// cancelOnClose releases a request's context once its body is done.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// forwardRetryAfter passes the upstream's Retry-After on with a 429 or 503 the
// client receives, so it knows when to come back.
func forwardRetryAfter(w http.ResponseWriter, resp *http.Response) {
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if v := resp.Header.Get("Retry-After"); v != "" {
			w.Header().Set("Retry-After", v)
		}
	}
}

// forwardRateLimits passes on the upstream's rate-limit state with a successful
// response: Anthropic's anthropic-ratelimit-* (which Claude Code reads for plan
// usage) and the x-ratelimit-* that OpenAI, Azure and OpenRouter send. resp is
// the deployment that answered, so a failed-over attempt's state never reaches
// the client. net/http has already canonicalised the names.
func forwardRateLimits(w http.ResponseWriter, resp *http.Response) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	for name, values := range resp.Header {
		if strings.HasPrefix(name, "Anthropic-Ratelimit-") || strings.HasPrefix(name, "X-Ratelimit-") {
			w.Header()[name] = values
		}
	}
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
func (g *Server) streamSSE(w http.ResponseWriter, resp *http.Response, dropUsageChunk bool, capture int, ttftStart time.Time) (usageCounts, string) {
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
			if output.Len() < capture && bytes.Contains(data, []byte(`"content"`)) {
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
func copyJSON(w http.ResponseWriter, resp *http.Response, capture int) (usageCounts, string) {
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
			if capture > 0 && len(parsed.Choices) > 0 && parsed.Choices[0].Message.Content != nil {
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
	Trace     traceCtx
	System    json.RawMessage // Anthropic's system, when captured
	Truncated bool            // captured content was cut
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
		CacheWrite5m:     counts.Write5m,
		CacheWrite1h:     counts.Write1h,
		LatencyMS:        int(time.Since(start).Milliseconds()),
		StatusCode:       status,
		Stream:           body.Stream,
		TTFTMs:           counts.TTFTMs,
		Attempts:         extra.Attempts,
		ErrorCode:        extra.ErrorCode,
		System:           extra.System,
		ContentTruncated: extra.Truncated,
		TraceID:          extra.Trace.TraceID,
		ParentSpanID:     extra.Trace.ParentID,
		Operation:        extra.Trace.Operation,
		InferenceGeo:     counts.Geo,
		Speed:            counts.Speed,
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
		tokens := pricing.Tokens{Prompt: counts.Prompt, Completion: counts.Completion, Cached: counts.Cached,
			Write5m: counts.Write5m, Write1h: counts.Write1h, USInference: counts.USInference, Fast: counts.Fast}
		e.CostUSD, e.Unpriced = pricing.Cost(snap, d, tokens)
		e.CostBasis = usage.CostBasisCurrent
		if e.Unpriced {
			// No real money changed hands, but the tokens still have a list value.
			e.NotionalCostUSD = pricing.Notional(snap, d, e.ModelName, tokens)
		}
	}
	g.Usage.Write(e)
	return e
}
