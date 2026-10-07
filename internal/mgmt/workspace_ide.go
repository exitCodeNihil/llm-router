package mgmt

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/workspace"
)

// idePrefix is stripped before proxying, so code-server sees itself at the
// root and its own absolute asset paths keep working.
const idePrefix = "/ide"

// workspaceIDE proxies the browser to the workspace's code-server.
//
// code-server runs with no password of its own — it is reachable only through
// here, and only after requireWorkspaceAccess and the owner check in
// loadWorkspace have both passed. On Docker it is published to 127.0.0.1, or not
// at all on a shared network (LLMR_WORKSPACE_NETWORK); on Kubernetes it is not
// published either.
//
// ReverseProxy has handled Connection: Upgrade since Go 1.20, which is what
// makes the IDE's terminal and language-server sockets work through it — that
// terminal is a real PTY, so ctrl+C and vim behave the way they should.
func (m *Server) workspaceIDE(w http.ResponseWriter, r *http.Request) {
	ws, ok := m.loadWorkspace(w, r)
	if !ok {
		return
	}
	// The endpoint is looked up on every request rather than cached: a host
	// port is not an identity, and after a daemon restart (or an OOM kill) a
	// cached port can belong to someone else's container. The runtime client
	// itself is cached, so this is a single inspect call.
	rt, _, err := m.cachedRuntime(r.Context())
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	endpoint, err := rt.IDEEndpoint(r.Context(), sandboxName(ws.ID))
	if err != nil {
		httpError(w, http.StatusBadGateway, "workspace IDE unavailable: "+err.Error())
		return
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	base := "/api/workspaces/" + ws.ID.String() + idePrefix
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// code-server serves from the root, so the path it is mounted at
			// here has to come off. A bare .../ide (no trailing slash) would
			// otherwise become an empty path.
			p := strings.TrimPrefix(pr.In.URL.Path, base)
			if p == "" {
				p = "/"
			}
			pr.Out.URL.Path = p
			pr.Out.URL.RawPath = ""
			// Its websockets check Origin against Host; both now refer to the
			// upstream, so they agree.
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Origin", target.Scheme+"://"+target.Host)
			stripCallerCredentials(pr.Out.Header)
		},
		// No FlushInterval tuning needed for websockets, but streamed HTTP
		// responses (file downloads, long polls) should not sit in a buffer.
		FlushInterval: 200 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpError(w, http.StatusBadGateway, "workspace IDE unreachable: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// stripCallerCredentials removes everything that authenticates the owner
// before a request enters the container. The container is the untrusted side
// of this proxy; code-server runs with --auth none and needs none of these.
func stripCallerCredentials(h http.Header) {
	for _, name := range []string{"Cookie", "Authorization", "X-Llmr-Key", "X-Api-Key"} {
		h.Del(name)
	}
}

// cachedRuntime is runtime() behind a short TTL: the IDE proxy calls it on
// every asset request, and the settings row it reads changes about never.
func (m *Server) cachedRuntime(ctx context.Context) (workspace.Runtime, workspace.Config, error) {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	if m.rt != nil && time.Since(m.rtAt) < 30*time.Second {
		return m.rt, m.rtCfg, nil
	}
	rt, cfg, err := m.runtime(ctx)
	if err != nil {
		return nil, cfg, err
	}
	m.rt, m.rtCfg, m.rtAt = rt, cfg, time.Now()
	return rt, cfg, nil
}

// forgetRuntime drops the cached client so a settings change takes effect
// on the next IDE request rather than after the TTL.
func (m *Server) forgetRuntime() {
	m.rtMu.Lock()
	m.rt = nil
	m.rtMu.Unlock()
}

// codeServerSettings is the IDE's user settings, written on a workspace's
// first start (they live under $HOME on the volume, so later edits stick).
// The colours are the console's own dark palette, so the editor reads as part
// of the same application rather than a stock VS Code in a frame.
const codeServerSettings = `{
  "workbench.colorTheme": "Default Dark Modern",
  "workbench.startupEditor": "none",
  "workbench.colorCustomizations": {
    "editor.background": "#10141c",
    "editorGutter.background": "#10141c",
    "sideBar.background": "#090b10",
    "sideBar.border": "#212838",
    "sideBarSectionHeader.background": "#090b10",
    "activityBar.background": "#090b10",
    "activityBar.border": "#212838",
    "activityBar.activeBorder": "#f5b84b",
    "activityBarBadge.background": "#f5b84b",
    "activityBarBadge.foreground": "#101010",
    "titleBar.activeBackground": "#090b10",
    "titleBar.border": "#212838",
    "statusBar.background": "#10141c",
    "statusBar.border": "#212838",
    "statusBar.noFolderBackground": "#10141c",
    "statusBarItem.remoteBackground": "#171d28",
    "panel.background": "#090b10",
    "panel.border": "#212838",
    "terminal.background": "#090b10",
    "editorGroupHeader.tabsBackground": "#090b10",
    "tab.activeBackground": "#10141c",
    "tab.inactiveBackground": "#090b10",
    "tab.border": "#212838",
    "tab.activeBorderTop": "#f5b84b",
    "list.activeSelectionBackground": "#171d28",
    "list.hoverBackground": "#171d28",
    "input.background": "#090b10",
    "input.border": "#2d3648",
    "dropdown.background": "#10141c",
    "focusBorder": "#f5b84b",
    "button.background": "#f5b84b",
    "button.foreground": "#101010",
    "button.hoverBackground": "#d9a22e",
    "progressBar.background": "#f5b84b",
    "textLink.foreground": "#4a9eed",
    "editorCursor.foreground": "#f5b84b",
    "editorLineNumber.activeForeground": "#f5b84b",
    "notifications.background": "#10141c",
    "widget.border": "#212838"
  },
  "terminal.integrated.enablePersistentSessions": true,
  "terminal.integrated.persistentSessionScrollback": 5000,
  "chat.disableAIFeatures": true,
  "workbench.secondarySideBar.defaultVisibility": "hidden",
  "security.workspace.trust.enabled": false,
  "telemetry.telemetryLevel": "off",
  "update.mode": "none",
  "extensions.autoUpdate": false
}
`

// codeServerSettingsFile is where code-server reads its user settings.
const codeServerSettingsFile = ".home/.local/share/code-server/User/settings.json"
