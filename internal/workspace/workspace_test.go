package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestSafePath(t *testing.T) {
	// Anything that resolves inside the root is fine, however it was written.
	ok := map[string]string{
		"":                  Root,
		"main.go":           Root + "/main.go",
		"/main.go":          Root + "/main.go",
		"src/../src/a.ts":   Root + "/src/a.ts",
		".home/.ssh/id_rsa": Root + "/.home/.ssh/id_rsa",
		// Traversal is re-anchored under the root rather than rejected, which
		// is equally safe and keeps the common "/workspace/x" spelling working.
		"../../etc/passwd":        Root + "/etc/passwd",
		"a/../../../../etc/hosts": Root + "/etc/hosts",
	}
	for in, want := range ok {
		got, err := SafePath(in)
		if err != nil {
			t.Fatalf("SafePath(%q) errored: %v", in, err)
		}
		if got != want {
			t.Errorf("SafePath(%q) = %q, want %q", in, got, want)
		}
		if got != Root && !strings.HasPrefix(got, Root+"/") {
			t.Errorf("SafePath(%q) escaped the root: %q", in, got)
		}
	}

	// A NUL byte would truncate the path inside the runtime's C string.
	if _, err := SafePath("ok\x00/../../etc/shadow"); err == nil {
		t.Error("expected NUL byte in path to be rejected")
	}
}

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		b := make([]byte, 8+len(s))
		b[0] = stream
		binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
		copy(b[8:], s)
		return b
	}

	var stdout, stderr strings.Builder
	collect := func(stream byte, p []byte) error {
		if stream == 2 {
			stderr.Write(p)
		} else {
			stdout.Write(p)
		}
		return nil
	}

	stream := bytes.Join([][]byte{
		frame(1, "hello "),
		frame(2, "warning"),
		frame(1, "world"),
	}, nil)
	if err := Demux(bytes.NewReader(stream), collect); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if stdout.String() != "hello world" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello world")
	}
	if stderr.String() != "warning" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "warning")
	}

	// A frame split across reads must still reassemble: io.ReadFull is what
	// makes this work, and a plain Read here would silently truncate output.
	stdout.Reset()
	stderr.Reset()
	full := frame(1, "abcdefghij")
	if err := Demux(io.MultiReader(
		bytes.NewReader(full[:5]),
		bytes.NewReader(full[5:12]),
		bytes.NewReader(full[12:]),
	), collect); err != nil {
		t.Fatalf("split demux: %v", err)
	}
	if stdout.String() != "abcdefghij" {
		t.Errorf("split stdout = %q, want %q", stdout.String(), "abcdefghij")
	}

	// A header that stops mid-way means the runtime cut us off — report it
	// rather than pretending the command produced all its output.
	if err := Demux(bytes.NewReader([]byte{1, 0, 0, 0, 5}), collect); err == nil {
		t.Error("expected truncated header to error")
	}
	// Same for a payload shorter than its header claims.
	if err := Demux(bytes.NewReader(append(frame(1, "")[:8], 'a')), collect); err == nil {
		t.Error("expected truncated payload to error")
	}
}

// testClient starts an HTTP server on a unix socket and returns a Client
// pointed at it, so request shaping is verified without a container runtime.
func testClient(t *testing.T, h http.HandlerFunc) *Docker {
	t.Helper()
	// Short path on purpose: macOS caps unix socket paths at ~104 bytes and
	// t.TempDir() under /var/folders is long enough to matter.
	dir, err := os.MkdirTemp("/tmp", "llmrws")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := dir + "/s"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewDocker(sock, "")
}

func TestCreateRequest(t *testing.T) {
	var gotPath string
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"Id":"abc123"}`))
	})

	err := c.create(context.Background(), "llmr-ws-1", Spec{
		Image: "node:22-slim", Env: []string{"HOME=" + HomeDir}, MemoryMB: 512, CPUs: 1.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if gotPath != "/containers/create?name=llmr-ws-1" {
		t.Errorf("path = %q", gotPath)
	}

	host, _ := body["HostConfig"].(map[string]any)
	if host == nil {
		t.Fatal("no HostConfig in create body")
	}
	// The volume bind is what makes anything persist at all.
	binds, _ := host["Binds"].([]any)
	if len(binds) != 1 || binds[0] != "llmr-ws-1:"+Root {
		t.Errorf("Binds = %v, want [llmr-ws-1:%s]", binds, Root)
	}
	if host["Memory"] != float64(512<<20) {
		t.Errorf("Memory = %v, want %v", host["Memory"], 512<<20)
	}
	if host["NanoCpus"] != float64(1.5e9) {
		t.Errorf("NanoCpus = %v, want 1.5e9", host["NanoCpus"])
	}
	// Dropping capabilities is a security property, so pin it in a test.
	caps, _ := host["CapDrop"].([]any)
	if len(caps) != 1 || caps[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", caps)
	}
	if body["WorkingDir"] != Root {
		t.Errorf("WorkingDir = %v, want %s", body["WorkingDir"], Root)
	}
}

// code-server has no password, so how it is exposed is a security property:
// loopback only by default, and not published at all on a shared network.
func TestCreateExposure(t *testing.T) {
	create := func(network string) (host map[string]any, c *Docker) {
		var body map[string]any
		c = testClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&body)
			w.Write([]byte(`{"Id":"abc123"}`))
		})
		c.network = network
		if err := c.create(context.Background(), "llmr-ws-1", Spec{Image: "node:22-slim"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		host, _ = body["HostConfig"].(map[string]any)
		return host, c
	}

	host, _ := create("")
	bind, _ := host["PortBindings"].(map[string]any)[IDEPort+"/tcp"].([]any)
	if len(bind) != 1 || bind[0].(map[string]any)["HostIp"] != "127.0.0.1" {
		t.Errorf("PortBindings = %v, want one binding on 127.0.0.1", host["PortBindings"])
	}
	if _, ok := host["NetworkMode"]; ok {
		t.Errorf("NetworkMode = %v, want none without a shared network", host["NetworkMode"])
	}

	host, c := create("llm-router-workspaces")
	if host["NetworkMode"] != "llm-router-workspaces" {
		t.Errorf("NetworkMode = %v", host["NetworkMode"])
	}
	if _, ok := host["PortBindings"]; ok {
		t.Errorf("PortBindings = %v, want nothing published on a shared network", host["PortBindings"])
	}
	if got, err := c.IDEEndpoint(context.Background(), "llmr-ws-1"); err != nil || got != "http://llmr-ws-1:"+IDEPort {
		t.Errorf("IDEEndpoint = %q, %v", got, err)
	}
}

func TestExecStreamsOutput(t *testing.T) {
	var execBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			json.NewDecoder(r.Body).Decode(&execBody)
			w.Write([]byte(`{"Id":"e1"}`))
		case r.URL.Path == "/exec/e1/json":
			w.Write([]byte(`{"ExitCode":3}`))
		case r.URL.Path == "/exec/e1/start":
			for _, f := range []struct {
				s byte
				b string
			}{{1, "out"}, {2, "err"}} {
				hdr := make([]byte, 8)
				hdr[0] = f.s
				binary.BigEndian.PutUint32(hdr[4:], uint32(len(f.b)))
				w.Write(append(hdr, f.b...))
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	var out, errOut strings.Builder
	code, err := c.Exec(context.Background(), "c1", []string{"ls", "-la"}, "", func(s byte, p []byte) error {
		if s == 2 {
			errOut.Write(p)
		} else {
			out.Write(p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out.String() != "out" || errOut.String() != "err" {
		t.Errorf("stdout=%q stderr=%q, want out/err", out.String(), errOut.String())
	}
	// A failed command must be distinguishable from one that printed nothing.
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	// Tty must stay false or the streams merge and the demuxer sees garbage.
	if execBody["Tty"] != false {
		t.Errorf("Tty = %v, want false", execBody["Tty"])
	}
	if execBody["WorkingDir"] != Root {
		t.Errorf("WorkingDir = %v, want %s", execBody["WorkingDir"], Root)
	}
}

func TestWriteFilesTar(t *testing.T) {
	var gotQuery string
	var raw []byte
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("path")
		raw, _ = io.ReadAll(r.Body)
	})

	err := c.WriteFiles(context.Background(), "c1", []File{
		{Path: ".home/.ssh/id_ed25519", Mode: 0o600, Data: []byte("KEY")},
		{Path: "README.md", Data: []byte("hi")},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if gotQuery != Root {
		t.Errorf("extract path = %q, want %q", gotQuery, Root)
	}

	found := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		found[h.Name] = h
	}
	// Names must be relative to the extraction root, not absolute, or they
	// unpack to the wrong place.
	key := found[".home/.ssh/id_ed25519"]
	if key == nil {
		t.Fatalf("key missing from tar; got %v", found)
	}
	// ssh refuses a private key that is group- or world-readable.
	if key.Mode != 0o600 {
		t.Errorf("key mode = %o, want 600", key.Mode)
	}
	if r := found["README.md"]; r == nil || r.Mode != 0o644 {
		t.Errorf("README = %+v, want mode 644", r)
	}
}

func TestImageAllowed(t *testing.T) {
	cfg := Config{DefaultImage: "llmr-workspace-base", Images: []string{"llmr-workspace-python"}}

	// A free-text image would let any allow-listed user make the control plane
	// pull arbitrary registry content, so non-admins are held to the list.
	for _, img := range []string{"llmr-workspace-base", "llmr-workspace-python", ""} {
		if !cfg.ImageAllowed(false, img) {
			t.Errorf("ImageAllowed(user, %q) = false, want true", img)
		}
	}
	for _, img := range []string{"evil/miner:latest", "ubuntu", "llmr-workspace-base:evil"} {
		if cfg.ImageAllowed(false, img) {
			t.Errorf("ImageAllowed(user, %q) = true, want false", img)
		}
		if !cfg.ImageAllowed(true, img) {
			t.Errorf("ImageAllowed(admin, %q) = false, want true", img)
		}
	}

	// With nothing configured, only the (empty) default passes — never a
	// wildcard.
	if (Config{}).ImageAllowed(false, "anything") {
		t.Error("empty config must not allow an arbitrary image")
	}
}

func TestConfigAllows(t *testing.T) {
	user, team := "u1", "t1"
	cases := []struct {
		name  string
		cfg   Config
		admin bool
		user  string
		teams []string
		want  bool
	}{
		{"disabled blocks everyone including admins",
			Config{Enabled: false, Allow: []Scope{{"user", user}}}, true, user, nil, false},
		{"empty allow-list means admins only",
			Config{Enabled: true}, true, "admin", nil, true},
		{"empty allow-list denies non-admins",
			Config{Enabled: true}, false, user, []string{team}, false},
		{"user scope matches",
			Config{Enabled: true, Allow: []Scope{{"user", user}}}, false, user, nil, true},
		{"user scope does not match a different user",
			Config{Enabled: true, Allow: []Scope{{"user", "other"}}}, false, user, nil, false},
		{"team scope matches one of the caller's teams",
			Config{Enabled: true, Allow: []Scope{{"team", team}}}, false, user, []string{"tx", team}, true},
		{"team scope does not match other teams",
			Config{Enabled: true, Allow: []Scope{{"team", "tz"}}}, false, user, []string{team}, false},
		// An empty scope value must never act as a wildcard.
		{"blank scope value matches nothing",
			Config{Enabled: true, Allow: []Scope{{"user", ""}}}, false, "", nil, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.Allows(tc.admin, tc.user, tc.teams); got != tc.want {
			t.Errorf("%s: Allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}
