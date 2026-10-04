// Package workspace runs developer workspaces: one sandbox per workspace, on
// either a local container runtime or Kubernetes.
package workspace

import (
	"context"
	"errors"
	"path"
	"strings"
)

// Root is where a workspace's volume is mounted inside its sandbox. Every
// client-supplied path is resolved under it and cannot escape.
const Root = "/workspace"

// IDEPort is where code-server listens inside every workspace image.
const IDEPort = "8080"

// HomeDir sits on the same volume as the code, which is the whole reason
// restarts keep anything: pi stores its sessions under $HOME, and so do the
// uploaded ssh keys and git config. Leave $HOME on the image and every chat is
// lost the moment the sandbox is recreated.
const HomeDir = Root + "/.home"

var errBadPath = errors.New("path escapes the workspace")

// Name is the sandbox's handle: container name, pod name and volume/PVC name
// all derive from the workspace id. Deriving rather than storing means cleanup
// never depends on a lookup, and both runtimes address a workspace the same way.
func Name(workspaceID string) string { return "llmr-ws-" + workspaceID }

// Spec describes the sandbox a workspace needs.
type Spec struct {
	WorkspaceID string
	Image       string
	// Env is applied whenever the sandbox is (re)created: Docker sets it on
	// the container, Kubernetes stores it in a Secret the Pod reads.
	Env      []string
	MemoryMB int
	CPUs     float64
	DiskGB   int
}

// File is one entry to write into a workspace.
type File struct {
	Path string // relative to Root
	Mode int64  // 0 falls back to 0644
	Data []byte
}

// Runtime is where a workspace actually runs. Two implementations: Docker (and
// podman, which serves the same API) and Kubernetes.
//
// Normally an interface with one implementation is a smell; here there are two
// genuinely different backends and the handlers must not know which they have.
// Builder is implemented by runtimes that can build an image from a Dockerfile
// (Docker/podman). Kubernetes cannot: images are built and pushed elsewhere.
type Builder interface {
	Build(ctx context.Context, tag, dockerfile string, out func(line string) error) error
}

type Runtime interface {
	// Ensure creates the sandbox if absent and leaves it running. It is
	// idempotent — calling it on a running workspace does nothing.
	Ensure(ctx context.Context, spec Spec) error
	// Stop leaves the workspace's data intact. Stopping something that was
	// never created succeeds: it is already true.
	Stop(ctx context.Context, name string) error
	// Remove deletes the sandbox and its data.
	Remove(ctx context.Context, name string) error
	Running(ctx context.Context, name string) (bool, error)
	// IDEEndpoint is the base URL of the workspace's code-server, reachable
	// from the control plane. The gateway proxies to it; nothing else may.
	IDEEndpoint(ctx context.Context, name string) (string, error)

	WriteFiles(ctx context.Context, name string, files []File) error
	// Exec runs argv and streams output, returning the exit code. stream is 1
	// for stdout and 2 for stderr.
	Exec(ctx context.Context, name string, argv []string, dir string, out func(stream byte, p []byte) error) (int, error)
}

// SafePath resolves a client-supplied path inside the workspace root.
//
// Prefixing with "/" before Clean is what makes this safe: "../../etc/passwd"
// cleans to "/etc/passwd", which Join then re-anchors under the root rather
// than escaping it. The explicit prefix check afterwards is belt-and-braces.
//
// Symlinks inside the sandbox are resolved on the far side and could point
// outside the root, but that is not an escalation: the workspace's owner can
// already run arbitrary commands in their own sandbox.
func SafePath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", errBadPath
	}
	full := path.Join(Root, path.Clean("/"+strings.TrimPrefix(p, "/")))
	if full != Root && !strings.HasPrefix(full, Root+"/") {
		return "", errBadPath
	}
	return full, nil
}

// tarName converts a workspace-relative path to its name inside a tar archive
// unpacked at Root.
func tarName(p string) (string, error) {
	full, err := SafePath(p)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(full, Root+"/"), nil
}

func fileMode(m int64) int64 {
	if m == 0 {
		return 0o644
	}
	return m
}
