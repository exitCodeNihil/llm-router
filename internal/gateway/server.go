// Package gateway implements the OpenAI-compatible data plane: auth, limits,
// routing, streaming proxy, and usage capture.
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/provider"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// LimitChecker is implemented by the limits package (M2). Nil means no limits.
type LimitChecker interface {
	// Check returns (allowed, status, code, message).
	Check(id *auth.Identity, s *snapshot.Snapshot) (bool, int, string, string)
	// RecordUsage feeds TPM buckets and budget deltas after a request completes.
	RecordUsage(id *auth.Identity, tokens int, costUSD float64)
}

// CloudTokenValidator is implemented by the auth package for Entra/GCP inbound
// tokens (M4). Nil means only gateway API keys are accepted.
type CloudTokenValidator func(ctx context.Context, s *snapshot.Snapshot, token string) (*auth.Identity, error)

type Server struct {
	Snapshots  *snapshot.Holder
	Usage      usage.Writer
	Limits     LimitChecker
	CloudAuth  CloudTokenValidator
	EdgeNodeID string // set in mode=gateway, stamped on events
}

type ctxKey int

const identityKey ctxKey = 0

func IdentityFrom(ctx context.Context) *auth.Identity {
	id, _ := ctx.Value(identityKey).(*auth.Identity)
	return id
}

// Register mounts the /v1 routes onto mux.
func (g *Server) Register(mux *http.ServeMux) {
	proxy := g.authenticate(http.HandlerFunc(g.handleProxy))
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.Handle("POST /v1/completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)
	mux.Handle("GET /v1/models", g.authenticate(http.HandlerFunc(g.handleModels)))
	// Anthropic Messages protocol (Claude Code, Anthropic SDKs)
	mux.Handle("POST /v1/messages", g.authenticate(http.HandlerFunc(g.handleAnthropicMessages)))
	mux.Handle("POST /v1/messages/count_tokens", g.authenticate(http.HandlerFunc(g.handleCountTokens)))
}

func (g *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := g.Snapshots.Get()
		if snap == nil {
			writeError(w, http.StatusServiceUnavailable, "server_error", "starting_up", "gateway has no config snapshot yet")
			return
		}
		// X-Llmr-Key first: pass-through clients need Authorization left free to
		// carry their own upstream credential (see the oauth_passthrough mode).
		token, viaAuthz := r.Header.Get("X-Llmr-Key"), false
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			viaAuthz = token != ""
		}
		if token == "" {
			token = r.Header.Get("x-api-key") // Anthropic SDK convention
		}
		if token == "" {
			writeAuthError(w, "missing API key: send Authorization, x-api-key, or X-Llmr-Key")
			return
		}

		var id *auth.Identity
		if strings.HasPrefix(token, auth.KeyPrefix) {
			id = auth.ResolveKey(snap, token)
		} else if g.CloudAuth != nil && strings.Count(token, ".") == 2 {
			var err error
			id, err = g.CloudAuth(r.Context(), snap, token)
			if err != nil {
				writeAuthError(w, "invalid identity token: "+err.Error())
				return
			}
		}
		if id == nil {
			writeAuthError(w, "invalid API key")
			return
		}

		if g.Limits != nil {
			if ok, status, code, msg := g.Limits.Check(id, snap); !ok {
				writeError(w, status, "insufficient_quota", code, msg)
				// A denial used to leave no trace at all, so "how often is this
				// key throttled" was unanswerable. The check runs before the body
				// is parsed, so model_name is necessarily blank here.
				g.emit(snap, id, nil, &reqBody{}, requestID(), usageCounts{}, status,
					time.Now(), nil, "", telemetryDecision(snap, id),
					emitExtra{ErrorCode: code})
				return
			}
		}
		// Only these caller headers may reach an upstream, and Authorization only
		// when it wasn't the gateway key we just authenticated.
		in := provider.Inbound{AnthropicBeta: r.Header.Get("anthropic-beta")}
		if !viaAuthz {
			in.Authorization = r.Header.Get("Authorization")
		}
		ctx := provider.WithInbound(r.Context(), in)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, identityKey, id)))
	})
}

// ServeAuthed serves an already-authenticated /v1-style request on behalf of
// id, bypassing header auth — the console chat playground bridges session
// users here without an API key. r.URL.Path must be a /v1/... proxy path.
// Limits are enforced the same way authenticate does.
func (g *Server) ServeAuthed(w http.ResponseWriter, r *http.Request, id *auth.Identity) {
	snap := g.Snapshots.Get()
	if snap == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "starting_up", "gateway has no config snapshot yet")
		return
	}
	if g.Limits != nil {
		if ok, status, code, msg := g.Limits.Check(id, snap); !ok {
			writeError(w, status, "insufficient_quota", code, msg)
			return
		}
	}
	g.handleProxy(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
}

func (g *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	snap := g.Snapshots.Get()
	id := IdentityFrom(r.Context())
	// One list, two dialects: OpenAI clients read object/owned_by, Anthropic
	// clients (Claude Code's gateway model discovery) read type/display_name.
	type model struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		OwnedBy     string `json:"owned_by"`
	}
	models := []model{}
	hasCred := provider.HasCallerCredential(r.Context())
	for _, name := range snap.ModelNames() {
		if !id.ModelAllowed(name) {
			continue
		}
		// A pass-through model is only usable by a caller that brought its own
		// upstream token; don't advertise it to one that didn't.
		if !hasCred && snap.CallerCredentialOnly(name) {
			continue
		}
		models = append(models, model{ID: name, Object: "model", Type: "model", DisplayName: name, OwnedBy: "llm-router"})
	}
	var firstID, lastID any
	if len(models) > 0 {
		firstID, lastID = models[0].ID, models[len(models)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "data": models, "has_more": false, "first_id": firstID, "last_id": lastID,
	})
}

func requestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// Handler wraps the mux with request logging.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes protocol upgrades through. Wrapping a ResponseWriter hides
// every optional interface it implements, and without this one any handler
// under Logging that tries to switch protocols — a websocket, the workspace
// IDE proxy — fails with "non-Hijacker ResponseWriter".
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	// The status was never written through us, so record what an upgrade is.
	w.status = http.StatusSwitchingProtocols
	return h.Hijack()
}
