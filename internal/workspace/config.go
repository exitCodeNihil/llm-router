package workspace

import "errors"

// Config is the workspace connector (settings key 'workspaces'). It is a
// control-plane concern only, so it deliberately does not ride in the routing
// snapshot the gateway and edge nodes read.
type Config struct {
	Enabled bool `json:"enabled"`
	// Runtime selects the backend: "docker" (podman serves the same API) or
	// "kubernetes". Cluster credentials are NOT here — see K8sAuthFromEnv.
	Runtime string `json:"runtime"`
	// StorageClass and DiskGB size the per-workspace PVC (kubernetes only).
	// Empty StorageClass takes the cluster default.
	StorageClass string `json:"storage_class"`
	DiskGB       int    `json:"disk_gb"`
	// Socket moves per host — /run/user/<uid>/podman/podman.sock when rootless,
	// /var/run/docker.sock for Docker, and a per-boot temp path on macOS where
	// podman runs in a VM. Never hardcode it.
	Socket string `json:"socket"`
	// GatewayURL is how the workspace reaches the gateway from *inside* the
	// container, which is not the URL a browser uses: on podman that is
	// host.containers.internal, on Docker host.docker.internal.
	GatewayURL   string `json:"gateway_url"`
	DefaultImage string `json:"default_image"`
	// Images non-admins may choose from. Empty means DefaultImage only —
	// a free-text image reference lets anyone make the control plane pull
	// arbitrary registry content, which is a disk-fill and probing surface.
	Images   []string `json:"images"`
	MemoryMB int      `json:"memory_mb"`
	CPUs     float64  `json:"cpus"`
	// BudgetUSD caps what the agent in each workspace can spend. Without it
	// the per-workspace key is uncapped and "a runaway agent gets 429'd" is
	// not true of anything.
	BudgetUSD float64 `json:"budget_usd"`
	// MaxPerUser bounds how many workspaces one user can create; unbounded
	// creation is a straightforward way to fill the host's disk.
	MaxPerUser int     `json:"max_per_user"`
	Allow      []Scope `json:"allow"`
}

// ImageAllowed reports whether a caller may create a workspace from this image.
// Admins may name any image; everyone else picks from the configured set.
func (c Config) ImageAllowed(isAdmin bool, image string) bool {
	if isAdmin || image == "" {
		return true
	}
	if image == c.DefaultImage {
		return true
	}
	for _, i := range c.Images {
		if i == image {
			return true
		}
	}
	return false
}

// New builds the runtime this config selects. Kubernetes credentials come from
// the environment, so a misconfigured cluster fails here with a message that
// says which variable is missing rather than at the first workspace start.
func New(c Config) (Runtime, error) {
	switch c.Runtime {
	case "kubernetes", "k8s":
		auth, err := K8sAuthFromEnv()
		if err != nil {
			return nil, err
		}
		return NewK8s(auth, c.StorageClass, c.DiskGB)
	default:
		if c.Socket == "" {
			return nil, errors.New("no container socket configured")
		}
		return NewDocker(c.Socket), nil
	}
}

// Scope names who may use workspaces, mirroring the telemetry rules engine's
// scope shape so there is one mental model for "who does this apply to".
type Scope struct {
	ScopeType  string `json:"scope_type"`  // team | user
	ScopeValue string `json:"scope_value"` // id
}

// Allows reports whether a caller may use workspaces at all. It fails closed
// twice over: a disabled connector allows nobody, and an empty allow-list means
// admins only rather than everyone — the opposite default would silently open
// container execution to every account the moment the feature was switched on.
//
// This governs *access to the feature*. Reaching a particular workspace is a
// separate ownership check, because being allowed to use workspaces is not
// permission to touch someone else's.
func (c Config) Allows(isAdmin bool, userID string, teamIDs []string) bool {
	if !c.Enabled {
		return false
	}
	if isAdmin {
		return true
	}
	for _, s := range c.Allow {
		switch s.ScopeType {
		case "user":
			if s.ScopeValue != "" && s.ScopeValue == userID {
				return true
			}
		case "team":
			for _, t := range teamIDs {
				if s.ScopeValue != "" && s.ScopeValue == t {
					return true
				}
			}
		}
	}
	return false
}
