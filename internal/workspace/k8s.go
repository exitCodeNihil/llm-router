package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Standard in-cluster service account paths.
const (
	saDir   = "/var/run/secrets/kubernetes.io/serviceaccount"
	saToken = saDir + "/token"
	saCA    = saDir + "/ca.crt"
	saNS    = saDir + "/namespace"
)

// K8sAuth is how the control plane reaches the API server.
//
// These are cluster credentials — a client key here is root-on-cluster — so
// they come from the environment and the filesystem, never from the settings
// row. That matches LLMR_DATABASE_URL and LLMR_ENCRYPTION_KEY: the most
// dangerous values are the ones we refuse to keep in Postgres.
type K8sAuth struct {
	APIURL    string
	Token     string
	CAPEM     []byte
	ClientPEM []byte
	KeyPEM    []byte
	Insecure  bool
	Namespace string
}

// K8sAuthFromEnv resolves credentials, preferring an explicit configuration and
// falling back to the in-cluster service account.
//
//	LLMR_K8S_API          https://127.0.0.1:6443
//	LLMR_K8S_TOKEN        bearer token, or
//	LLMR_K8S_CLIENT_CERT  + LLMR_K8S_CLIENT_KEY   PEM file paths (mTLS)
//	LLMR_K8S_CA           PEM file path
//	LLMR_K8S_INSECURE     "1" to skip verification — development only
//	LLMR_K8S_NAMESPACE    defaults to the in-cluster namespace, else "default"
func K8sAuthFromEnv() (K8sAuth, error) {
	a := K8sAuth{
		APIURL:    os.Getenv("LLMR_K8S_API"),
		Token:     os.Getenv("LLMR_K8S_TOKEN"),
		Insecure:  os.Getenv("LLMR_K8S_INSECURE") == "1",
		Namespace: os.Getenv("LLMR_K8S_NAMESPACE"),
	}
	read := func(env string) ([]byte, error) {
		p := os.Getenv(env)
		if p == "" {
			return nil, nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		return b, nil
	}
	var err error
	if a.CAPEM, err = read("LLMR_K8S_CA"); err != nil {
		return a, err
	}
	if a.ClientPEM, err = read("LLMR_K8S_CLIENT_CERT"); err != nil {
		return a, err
	}
	if a.KeyPEM, err = read("LLMR_K8S_CLIENT_KEY"); err != nil {
		return a, err
	}

	// In-cluster: the service account is mounted, and the API server is in env.
	if a.APIURL == "" {
		if host := os.Getenv("KUBERNETES_SERVICE_HOST"); host != "" {
			port := os.Getenv("KUBERNETES_SERVICE_PORT")
			if port == "" {
				port = "443"
			}
			a.APIURL = "https://" + net_JoinHostPort(host, port)
		}
	}
	if a.Token == "" {
		if b, err := os.ReadFile(saToken); err == nil {
			a.Token = strings.TrimSpace(string(b))
		}
	}
	if a.CAPEM == nil {
		if b, err := os.ReadFile(saCA); err == nil {
			a.CAPEM = b
		}
	}
	if a.Namespace == "" {
		if b, err := os.ReadFile(saNS); err == nil {
			a.Namespace = strings.TrimSpace(string(b))
		}
	}
	if a.Namespace == "" {
		a.Namespace = "default"
	}
	if a.APIURL == "" {
		return a, errors.New("kubernetes runtime selected but LLMR_K8S_API is not set and no in-cluster service account was found")
	}
	if a.Token == "" && a.ClientPEM == nil {
		return a, errors.New("kubernetes runtime selected but no credentials: set LLMR_K8S_TOKEN or LLMR_K8S_CLIENT_CERT/LLMR_K8S_CLIENT_KEY")
	}
	return a, nil
}

// net.JoinHostPort without importing net for one call.
func net_JoinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// K8s runs each workspace as a Pod with a PVC, addressed by the workspace's
// derived name.
//
// Where Docker keeps a stopped container around, Kubernetes has no such state:
// Stop deletes the Pod and Ensure recreates it. The PVC — and therefore
// everything under /workspace — outlives both, which is what makes stop/start
// behave the same way to a user.
type K8s struct {
	auth         K8sAuth
	http         *http.Client
	storageClass string
	diskGB       int
}

func NewK8s(auth K8sAuth, storageClass string, diskGB int) (*K8s, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: auth.Insecure} //nolint:gosec // opt-in, documented as dev-only
	if len(auth.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(auth.CAPEM) {
			return nil, errors.New("LLMR_K8S_CA is not a valid PEM bundle")
		}
		tlsCfg.RootCAs = pool
	}
	if len(auth.ClientPEM) > 0 {
		cert, err := tls.X509KeyPair(auth.ClientPEM, auth.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("kubernetes client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if diskGB <= 0 {
		diskGB = 5
	}
	return &K8s{
		auth:         auth,
		storageClass: storageClass,
		diskGB:       diskGB,
		// No Client.Timeout — exec streams for as long as the command runs.
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// --- request plumbing ---

func (k *K8s) url(path string) string { return strings.TrimSuffix(k.auth.APIURL, "/") + path }

func (k *K8s) ns() string { return k.auth.Namespace }

func (k *K8s) req(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.url(path), r)
	if err != nil {
		return nil, err
	}
	if k.auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+k.auth.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (k *K8s) do(ctx context.Context, method, path string, body any, out any) error {
	req, err := k.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return k8sError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// k8sError unwraps the API's Status object so a failure reads as its actual
// reason rather than a bare status code.
func k8sError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var s struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(raw, &s) == nil && s.Message != "" {
		return fmt.Errorf("kubernetes %d (%s): %s", resp.StatusCode, s.Reason, s.Message)
	}
	return fmt.Errorf("kubernetes %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func isK8sNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "kubernetes 404")
}

// --- lifecycle ---

func (k *K8s) Ensure(ctx context.Context, spec Spec) error {
	name := Name(spec.WorkspaceID)

	running, err := k.Running(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	if err := k.ensurePVC(ctx, name); err != nil {
		return err
	}
	// Env lives in a Secret rather than the Pod spec so that recreating the
	// Pod — which is what "start" means here — does not need the caller to
	// still be holding the workspace's API key.
	if len(spec.Env) > 0 {
		if err := k.putSecret(ctx, name, spec.Env); err != nil {
			return err
		}
	}
	if err := k.ensurePod(ctx, name, spec); err != nil {
		return err
	}
	return k.waitReady(ctx, name)
}

func (k *K8s) ensurePVC(ctx context.Context, name string) error {
	err := k.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", k.ns(), name), nil, nil)
	if err == nil {
		return nil
	}
	if !isK8sNotFound(err) {
		return err
	}
	pvc := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "labels": labels(name)},
		"spec": map[string]any{
			// One Pod at a time uses this claim, so ReadWriteOnce is right and
			// is what every default StorageClass actually offers.
			"accessModes": []string{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", k.diskGB)}},
		},
	}
	if k.storageClass != "" {
		pvc["spec"].(map[string]any)["storageClassName"] = k.storageClass
	}
	return k.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims", k.ns()), pvc, nil)
}

func (k *K8s) putSecret(ctx context.Context, name string, env []string) error {
	data := map[string]string{}
	for _, e := range env {
		key, value, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "labels": labels(name)},
		"data":       data,
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets", k.ns())
	err := k.do(ctx, http.MethodPost, path, secret, nil)
	if err != nil && strings.Contains(err.Error(), "kubernetes 409") {
		// Already exists: replace, so a re-minted key actually takes effect.
		return k.do(ctx, http.MethodPut, path+"/"+name, secret, nil)
	}
	return err
}

func (k *K8s) ensurePod(ctx context.Context, name string, spec Spec) error {
	// A pod being deleted still answers GET, and its container can still report
	// ready — so a stop immediately followed by a start would adopt a corpse and
	// call the workspace running seconds before it disappears. Wait it out.
	if err := k.waitPodGone(ctx, name); err != nil {
		return err
	}
	err := k.do(ctx, http.MethodGet, k.podPath(name), nil, nil)
	if err == nil {
		return nil // already there, just not ready yet
	}
	if !isK8sNotFound(err) {
		return err
	}

	// Memory and CPU are ceilings, matching what they mean on Docker. Setting
	// only limits would make Kubernetes copy them into requests, turning the
	// workspace into a Guaranteed pod that *reserves* the whole allowance — on
	// a small node that fails to schedule at all, for a workspace that spends
	// most of its life idle at a shell prompt. Small requests, real limits.
	resources := map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
	}
	limits := map[string]any{}
	if spec.MemoryMB > 0 {
		limits["memory"] = fmt.Sprintf("%dMi", spec.MemoryMB)
	}
	if spec.CPUs > 0 {
		limits["cpu"] = fmt.Sprintf("%dm", int(spec.CPUs*1000))
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}

	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "labels": labels(name)},
		"spec": map[string]any{
			// The image's own command starts code-server; Always keeps the
			// workspace alive if it ever falls over.
			"restartPolicy": "Always",
			"containers": []any{map[string]any{
				"name":  "workspace",
				"image": spec.Image,
				// Kubernetes defaults a ":latest" tag to Always, which makes a
				// locally-built image unusable: it tries to pull it from a
				// registry that has never heard of it. Workspace images are
				// pinned or built on the node, so present-is-good-enough.
				"imagePullPolicy": "IfNotPresent",
				"ports":           []any{map[string]any{"containerPort": 8080, "name": "ide"}},
				"workingDir":      Root,
				"envFrom":         []any{map[string]any{"secretRef": map[string]any{"name": name}}},
				"resources":       resources,
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false,
					"capabilities":             map[string]any{"drop": []string{"ALL"}},
				},
				"volumeMounts": []any{map[string]any{"name": "work", "mountPath": Root}},
			}},
			"volumes": []any{map[string]any{
				"name":                  "work",
				"persistentVolumeClaim": map[string]any{"claimName": name},
			}},
		},
	}
	return k.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/namespaces/%s/pods", k.ns()), pod, nil)
}

// waitPodGone returns once no pod with this name is terminating. A pod that
// exists without a deletion timestamp is a live one and returns immediately —
// only a dying one is waited on.
func (k *K8s) waitPodGone(ctx context.Context, name string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var pod struct {
			Metadata struct {
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
		}
		err := k.do(ctx, http.MethodGet, k.podPath(name), nil, &pod)
		if isK8sNotFound(err) || (err == nil && pod.Metadata.DeletionTimestamp == "") {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("previous workspace pod is still terminating after 2m")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// waitReady blocks until the Pod can actually be exec'd into. Returning as soon
// as the Pod object exists would hand back a workspace whose very first request
// fails while the image is still pulling.
func (k *K8s) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for {
		ready, phase, err := k.podReady(ctx, name)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		last = phase
		if time.Now().After(deadline) {
			return fmt.Errorf("workspace pod is still %s after 3m — check image pull and PVC binding", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (k *K8s) podPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", k.ns(), name)
}

func (k *K8s) podReady(ctx context.Context, name string) (ready bool, phase string, err error) {
	var pod struct {
		Metadata struct {
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := k.do(ctx, http.MethodGet, k.podPath(name), nil, &pod); err != nil {
		if isK8sNotFound(err) {
			return false, "absent", nil
		}
		return false, "", err
	}
	// A terminating pod keeps reporting its containers ready right up until it
	// goes. Calling that running would hand back a workspace with seconds to
	// live, and would let Ensure skip recreating it.
	if pod.Metadata.DeletionTimestamp != "" {
		return false, "terminating", nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			return true, pod.Status.Phase, nil
		}
	}
	return false, pod.Status.Phase, nil
}

// IDEEndpoint returns the Pod's own address. Nothing is published: reaching it
// means the control plane is on the cluster network, which is true when
// llm-router runs in the cluster and false when it runs on a laptop against a
// remote API server — say so plainly rather than time out later.
func (k *K8s) IDEEndpoint(ctx context.Context, name string) (string, error) {
	var pod struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	if err := k.do(ctx, http.MethodGet, k.podPath(name), nil, &pod); err != nil {
		return "", err
	}
	if pod.Status.PodIP == "" {
		return "", errors.New("workspace pod has no address yet")
	}
	return "http://" + net_JoinHostPort(pod.Status.PodIP, IDEPort), nil
}

func (k *K8s) Running(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	ready, _, err := k.podReady(ctx, name)
	return ready, err
}

// Stop deletes the Pod and keeps the PVC. There is no stopped-Pod state in
// Kubernetes; the data surviving is what makes this equivalent to stopping a
// container.
func (k *K8s) Stop(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	err := k.do(ctx, http.MethodDelete, k.podPath(name), nil, nil)
	if isK8sNotFound(err) {
		return nil
	}
	return err
}

func (k *K8s) Remove(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	for _, path := range []string{
		k.podPath(name),
		fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", k.ns(), name),
		fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", k.ns(), name),
	} {
		if err := k.do(ctx, http.MethodDelete, path, nil, nil); err != nil && !isK8sNotFound(err) {
			return err
		}
	}
	return nil
}

// --- files ---

// WriteFiles pipes a tar into the Pod. Base64 keeps the archive out of shell
// argument parsing entirely — the payload is arbitrary bytes and must never be
// interpreted.
// k8sWriteLimit is what fits in an exec request line: the archive travels as
// a command argument, and the API server caps the request header at 1 MiB.
// ponytail: stream through the exec websocket's stdin to lift this.
const k8sWriteLimit = 512 << 10

func (k *K8s) WriteFiles(ctx context.Context, name string, files []File) error {
	buf, err := tarFiles(files)
	if err != nil {
		return err
	}
	if len(buf) > k8sWriteLimit {
		return fmt.Errorf("files total %d KB; the Kubernetes runtime can write at most %d KB per start — remove some stored files", len(buf)>>10, k8sWriteLimit>>10)
	}
	encoded := base64.StdEncoding.EncodeToString(buf)
	script := fmt.Sprintf("printf %%s '%s' | base64 -d | tar xf - -C %s", encoded, Root)

	var stderr bytes.Buffer
	code, err := k.Exec(ctx, name, []string{"sh", "-c", script}, Root, func(stream byte, b []byte) error {
		if stream == 2 {
			stderr.Write(b)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("write failed: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// --- exec ---

// Kubernetes stream channel bytes for the v4 remote-command protocol.
const (
	chStdout = 1
	chStderr = 2
	chError  = 3
)

// Exec runs argv in the Pod over the exec subresource.
//
// This is a WebSocket upgrade rather than plain streaming, which is the one
// place the two runtimes genuinely differ at the transport level. Go's
// Transport hands back an io.ReadWriteCloser body on a 101, so no raw dialing
// is needed — see wsframe.go for why the framing is hand-rolled.
func (k *K8s) Exec(ctx context.Context, name string, argv []string, dir string, out func(stream byte, p []byte) error) (int, error) {
	if dir == "" {
		dir = Root
	}
	// There is no WorkingDir on the exec subresource, so the directory has to
	// be part of the command.
	wrapped := []string{"sh", "-c", "cd " + shellQuote(dir) + " && exec \"$@\"", "sh"}
	wrapped = append(wrapped, argv...)

	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "stdin": {"false"}, "tty": {"false"}}
	for _, a := range wrapped {
		q.Add("command", a)
	}
	path := k.podPath(name) + "/exec?" + q.Encode()

	conn, err := k.dialWS(ctx, path)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	exit := 0
	for {
		msg, err := conn.readMessage()
		if err == io.EOF {
			return exit, nil
		}
		if err != nil {
			return 0, err
		}
		if len(msg) == 0 {
			continue
		}
		channel, payload := msg[0], msg[1:]
		switch channel {
		case chStdout, chStderr:
			if len(payload) > 0 {
				if err := out(channel, payload); err != nil {
					return 0, err
				}
			}
		case chError:
			// The error channel carries a Status object at the end of every
			// exec — success included. It is the only place the exit code is
			// reported, so a non-zero exit is invisible without parsing it.
			if code, ok := exitCodeFromStatus(payload); ok {
				exit = code
			}
		}
	}
}

// exitCodeFromStatus reads the exit code out of the terminating Status. A
// successful command reports Status "Success" and no code.
func exitCodeFromStatus(payload []byte) (int, bool) {
	var st struct {
		Status  string `json:"status"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if json.Unmarshal(payload, &st) != nil {
		return 0, false
	}
	if st.Status == "Success" {
		return 0, true
	}
	for _, c := range st.Details.Causes {
		if c.Reason == "ExitCode" {
			var code int
			if _, err := fmt.Sscanf(c.Message, "%d", &code); err == nil {
				return code, true
			}
		}
	}
	// A non-success status with no exit code still means the command failed.
	if st.Status != "" {
		return 1, true
	}
	return 0, false
}

func (k *K8s) dialWS(ctx context.Context, path string) (*wsConn, error) {
	req, err := k.req(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	// The exec subresource negotiates a stream, not a representation: leaving
	// the JSON Accept header on it gets a 406 with a list of representations
	// the apiserver would rather send.
	req.Header.Del("Accept")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(nonce[:]))
	req.Header.Set("Sec-WebSocket-Protocol", "v4.channel.k8s.io")

	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubernetes exec: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, k8sError(resp)
	}
	// Documented behaviour of net/http.Transport: on a 101 the body is a
	// read-write connection rather than a one-way stream.
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("kubernetes exec: connection is not writable")
	}
	return newWSConn(rwc), nil
}

// shellQuote makes a path safe to embed in a shell command. Paths come from
// SafePath so they are already confined, but quoting keeps a directory with a
// space or a quote in its name from becoming syntax.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func labels(name string) map[string]any {
	return map[string]any{"app.kubernetes.io/managed-by": "llm-router", "llmr.workspace": name}
}

var _ Runtime = (*K8s)(nil)
