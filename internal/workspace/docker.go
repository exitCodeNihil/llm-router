package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Docker talks to a container runtime over its Docker-compatible unix socket.
// Podman serves the same API, so one implementation covers both.
//
// ponytail: stdlib HTTP over the socket instead of the Docker SDK — we use ten
// endpoints, which is not worth forty transitive dependencies. Swap in the SDK
// only if we start needing the parts of the API that are genuinely awkward.
type Docker struct {
	http *http.Client
	// network names a user-defined network that the gateway and every workspace
	// share. Set, the IDE is reached by container name on that network and no
	// port is published; unset, it is published on the host's loopback, which
	// only works when the gateway runs on the host itself.
	network string
}

func NewDocker(socket, network string) *Docker {
	return &Docker{network: network, http: &http.Client{
		// No Client.Timeout: exec responses stream for as long as the command
		// runs. A deadline belongs on the request context, not here.
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// --- request plumbing ---

func (c *Docker) do(ctx context.Context, method, endpoint string, body io.Reader, ctype string) (*http.Response, error) {
	// The host is ignored — DialContext always lands on the socket — but
	// net/http still requires a syntactically valid URL.
	req, err := http.NewRequestWithContext(ctx, method, "http://d"+endpoint, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("container runtime unreachable: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

func (c *Docker) postJSON(ctx context.Context, endpoint string, in any) (*http.Response, error) {
	buf, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, endpoint, bytes.NewReader(buf), "application/json")
}

// apiError unwraps the runtime's {"message": "..."} error shape, falling back
// to the raw body so an unexpected error is never swallowed into "failed".
func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("runtime %d: %s", resp.StatusCode, e.Message)
	}
	return fmt.Errorf("runtime %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func isNotFound(err error) bool { return err != nil && strings.Contains(err.Error(), "404") }

// --- lifecycle ---

// Ensure creates the volume and container on first use, then starts it. The
// container runs `sleep infinity` because a workspace is a place to exec into,
// not a process to supervise — nothing here should die when a command finishes.
func (c *Docker) Ensure(ctx context.Context, spec Spec) error {
	name := Name(spec.WorkspaceID)

	running, err := c.Running(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	exists, err := c.exists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		// A stopped container is pinned to the image it was created from, so
		// a rebuilt template would never reach it. Recreate it on the current
		// image; the volume (and everything under $HOME) is untouched.
		stale, err := c.imageStale(ctx, name, spec.Image)
		if err != nil {
			return err
		}
		if stale {
			if err := c.removeContainer(ctx, name); err != nil {
				return err
			}
			exists = false
		}
	}
	if !exists {
		if err := c.createVolume(ctx, name); err != nil {
			return err
		}
		if err := c.create(ctx, name, spec); err != nil {
			return err
		}
	}
	return c.start(ctx, name)
}

// imageStale reports whether the container was created from an image other
// than the one image currently names.
func (c *Docker) imageStale(ctx context.Context, name, image string) (bool, error) {
	var ctr struct {
		Image string `json:"Image"`
	}
	if err := c.getJSON(ctx, "/containers/"+name+"/json", &ctr); err != nil {
		return false, err
	}
	var img struct {
		ID string `json:"Id"`
	}
	if err := c.getJSON(ctx, "/images/"+url.PathEscape(image)+"/json", &img); err != nil {
		if isNotFound(err) {
			return false, nil // image gone: keep what we have rather than fail
		}
		return false, err
	}
	return img.ID != "" && ctr.Image != img.ID, nil
}

func (c *Docker) getJSON(ctx context.Context, endpoint string, v any) error {
	resp, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// removeContainer drops the container and nothing else; Remove is the one
// that takes the volume with it.
func (c *Docker) removeContainer(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+name+"?force=true&v=false", nil, "")
	if err != nil && !isNotFound(err) {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}
	return nil
}

func (c *Docker) createVolume(ctx context.Context, name string) error {
	resp, err := c.postJSON(ctx, "/volumes/create", map[string]any{"Name": name})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Docker) create(ctx context.Context, name string, spec Spec) error {
	host := map[string]any{
		"Binds":       []string{name + ":" + Root},
		"CapDrop":     []string{"ALL"},
		"SecurityOpt": []string{"no-new-privileges"},
		"PidsLimit":   512,
	}
	// code-server has no password of its own, so it must not be reachable
	// from anywhere but the gateway's proxy. Either nothing is published and
	// only the shared network can reach it, or it is published on this host's
	// loopback, where an empty HostPort lets the runtime pick a free one.
	if c.network != "" {
		host["NetworkMode"] = c.network
	} else {
		host["PortBindings"] = map[string]any{
			IDEPort + "/tcp": []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": ""}},
		}
	}
	if spec.MemoryMB > 0 {
		host["Memory"] = int64(spec.MemoryMB) * 1 << 20
	}
	if spec.CPUs > 0 {
		host["NanoCpus"] = int64(spec.CPUs * 1e9)
	}
	resp, err := c.postJSON(ctx, "/containers/create?name="+url.QueryEscape(name), map[string]any{
		"Image": spec.Image,
		// No Cmd: the image starts code-server, which is what a workspace is.
		"WorkingDir":   Root,
		"Env":          spec.Env,
		"ExposedPorts": map[string]any{IDEPort + "/tcp": map[string]any{}},
		"HostConfig":   host,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Docker) start(ctx context.Context, name string) error {
	resp, err := c.postJSON(ctx, "/containers/"+name+"/start", nil)
	if err != nil {
		// Already running is success, not an error worth surfacing.
		if strings.Contains(err.Error(), "304") || strings.Contains(err.Error(), "already") {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Docker) exists(ctx context.Context, name string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+name+"/json", nil, "")
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

func (c *Docker) Stop(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	resp, err := c.postJSON(ctx, "/containers/"+name+"/stop?t=5", nil)
	if err != nil {
		// Not running, already stopped, or never created: all already true.
		if isNotFound(err) || strings.Contains(err.Error(), "304") || strings.Contains(err.Error(), "not running") {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Docker) Remove(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+name+"?force=true&v=false", nil, "")
	if err != nil && !isNotFound(err) {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}
	// The volume shares the container's name, so the workspace's data goes
	// with it without anything having to be looked up.
	resp, err = c.do(ctx, http.MethodDelete, "/volumes/"+name, nil, "")
	if err != nil && !isNotFound(err) {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}
	return nil
}

// Running asks the runtime rather than trusting stored state, so a container
// that died out from under us is never reported as up.
func (c *Docker) Running(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+name+"/json", nil, "")
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.State.Running, nil
}

// IDEEndpoint reads back the host port the runtime bound. Looking it up rather
// than storing it means a container recreated on a different port still
// resolves, and nothing has to be kept in sync.
func (c *Docker) IDEEndpoint(ctx context.Context, name string) (string, error) {
	if c.network != "" {
		return "http://" + net.JoinHostPort(name, IDEPort), nil
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+name+"/json", nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, b := range out.NetworkSettings.Ports[IDEPort+"/tcp"] {
		if b.HostPort != "" {
			return "http://127.0.0.1:" + b.HostPort, nil
		}
	}
	return "", errors.New("workspace has no published IDE port — recreate it")
}

// --- files, via the archive endpoint ---

// WriteFiles tars the entries and unpacks them at the workspace root. Mode is
// honoured because ssh refuses to use a private key that is not 0600.
func (c *Docker) WriteFiles(ctx context.Context, name string, files []File) error {
	buf, err := tarFiles(files)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut,
		"/containers/"+name+"/archive?path="+url.QueryEscape(Root),
		bytes.NewReader(buf), "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func readTarFile(r io.Reader, limit int64) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("not found in archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, limit))
	}
}

func tarFiles(files []File) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		name, err := tarName(f.Path)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: fileMode(f.Mode), Size: int64(len(f.Data)),
			Typeflag: tar.TypeReg, ModTime: time.Now(),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- exec ---

// Exec runs argv in the container and streams its output.
//
// Nothing here writes to stdin, which is what lets us avoid hijacking the
// connection: with Detach false the response body *is* the output stream.
// Tty stays false so stdout and stderr arrive separately — and as a bonus most
// tools suppress ANSI colour when stdout is not a terminal, so the output
// renders cleanly without a terminal emulator on the front end.
func (c *Docker) Exec(ctx context.Context, name string, argv []string, dir string, out func(stream byte, p []byte) error) (int, error) {
	if dir == "" {
		dir = Root
	}
	resp, err := c.postJSON(ctx, "/containers/"+name+"/exec", map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          argv,
		"WorkingDir":   dir,
	})
	if err != nil {
		return 0, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil {
		return 0, err
	}

	run, err := c.postJSON(ctx, "/exec/"+created.ID+"/start", map[string]any{
		"Detach": false, "Tty": false,
	})
	if err != nil {
		return 0, err
	}
	err = Demux(run.Body, out)
	run.Body.Close()
	if err != nil {
		return 0, err
	}
	return c.execExitCode(ctx, created.ID)
}

// execExitCode reads the exit status after the output stream ends. Failing to
// read it is not a failure of the command, so it degrades to 0 rather than
// turning a successful command into an error.
func (c *Docker) execExitCode(ctx context.Context, execID string) (int, error) {
	resp, err := c.do(ctx, http.MethodGet, "/exec/"+execID+"/json", nil, "")
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	var out struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, nil
	}
	return out.ExitCode, nil
}

// Demux splits the runtime's multiplexed exec stream. Each frame is an 8-byte
// header — stream type, three zero bytes, then a big-endian uint32 length —
// followed by that many payload bytes.
func Demux(r io.Reader, out func(stream byte, p []byte) error) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			// A clean EOF ends the stream; a partial header means the runtime
			// cut us off mid-frame, which is worth reporting.
			if err == io.EOF {
				return nil
			}
			if err == io.ErrUnexpectedEOF {
				return errors.New("truncated exec frame header")
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if n == 0 {
			continue
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return errors.New("truncated exec frame payload")
		}
		if err := out(hdr[0], payload); err != nil {
			return err
		}
	}
}

var _ Runtime = (*Docker)(nil)

// Build turns a Dockerfile into a tagged image on this runtime, streaming the
// daemon's build output to out. The build context is the Dockerfile alone:
// templates fetch what they need from the network, they do not COPY files.
func (c *Docker) Build(ctx context.Context, tag, dockerfile string, out func(line string) error) error {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	q := url.Values{"t": {tag}, "dockerfile": {"Dockerfile"}, "rm": {"1"}}
	resp, err := c.do(ctx, http.MethodPost, "/build?"+q.Encode(), &tarBuf, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body is a stream of JSON objects; "stream" carries progress text and
	// "error" a failed step. The HTTP status is 200 either way.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("build stream: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("build failed: %s", strings.TrimSpace(msg.Error))
		}
		if msg.Stream != "" {
			if err := out(msg.Stream); err != nil {
				return err
			}
		}
	}
}
