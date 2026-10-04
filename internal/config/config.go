// Package config holds process configuration read from flags and environment.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Mode          string // "all" or "gateway"
	Listen        string // address for the public gateway + console + mgmt API
	DatabaseURL   string
	AdminToken    string // bootstrap admin token for mgmt API
	EncryptionKey string // hex/plain 32-byte key for provider secret encryption
	RedisURL      string

	// IDEListen/IDEOrigin put the workspace IDE proxy on its own origin so
	// content rendered by a container is never same-origin with the console.
	// Both empty = the IDE is served from the main listener (not recommended).
	IDEListen string
	IDEOrigin string
	// CookieDomain scopes the session cookie (e.g. ".example.com") so an IDE
	// origin on a sibling hostname receives it. Empty = host-only cookie,
	// which is right when the IDE differs from the console by port alone.
	CookieDomain string

	// gateway (edge) mode
	ControlPlaneURL string
	NodeToken       string
	SnapshotCache   string
}

func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("llmrouter", flag.ContinueOnError)
	fs.StringVar(&c.Mode, "mode", envOr("LLMR_MODE", "all"), "run mode: all | gateway")
	fs.StringVar(&c.Listen, "listen", envOr("LLMR_LISTEN", ":8080"), "listen address")
	fs.StringVar(&c.DatabaseURL, "database-url", os.Getenv("LLMR_DATABASE_URL"), "postgres URL (mode=all)")
	fs.StringVar(&c.ControlPlaneURL, "control-plane-url", os.Getenv("LLMR_CONTROL_PLANE_URL"), "control plane base URL (mode=gateway)")
	fs.StringVar(&c.SnapshotCache, "snapshot-cache", envOr("LLMR_SNAPSHOT_CACHE", "/var/lib/llmrouter/snapshot.json"), "on-disk snapshot cache path (mode=gateway)")
	fs.StringVar(&c.IDEListen, "ide-listen", os.Getenv("LLMR_IDE_LISTEN"), "listen address for the workspace IDE proxy (mode=all)")
	fs.StringVar(&c.IDEOrigin, "ide-origin", os.Getenv("LLMR_IDE_ORIGIN"), "public origin of the IDE listener, e.g. http://localhost:8081")
	fs.StringVar(&c.CookieDomain, "cookie-domain", os.Getenv("LLMR_COOKIE_DOMAIN"), "session cookie Domain attribute, e.g. .example.com (needed when the IDE origin is a different hostname)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.AdminToken = os.Getenv("LLMR_ADMIN_TOKEN")
	c.EncryptionKey = os.Getenv("LLMR_ENCRYPTION_KEY")
	c.RedisURL = os.Getenv("LLMR_REDIS_URL")
	c.NodeToken = os.Getenv("LLMR_NODE_TOKEN")

	switch c.Mode {
	case "all":
		if c.DatabaseURL == "" {
			return nil, fmt.Errorf("LLMR_DATABASE_URL is required in mode=all")
		}
		// The encryption key also signs console sessions: an empty or
		// placeholder value lets anyone mint a session for any user id.
		if err := checkSecret("LLMR_ENCRYPTION_KEY", c.EncryptionKey, true, 16); err != nil {
			return nil, err
		}
		if err := checkSecret("LLMR_ADMIN_TOKEN", c.AdminToken, false, 12); err != nil {
			return nil, err
		}
		if (c.IDEListen == "") != (c.IDEOrigin == "") {
			return nil, fmt.Errorf("LLMR_IDE_LISTEN and LLMR_IDE_ORIGIN must be set together")
		}
		if c.IDEOrigin != "" && !strings.HasPrefix(c.IDEOrigin, "http://") && !strings.HasPrefix(c.IDEOrigin, "https://") {
			return nil, fmt.Errorf("LLMR_IDE_ORIGIN must be an origin like https://ide.example.com")
		}
		c.IDEOrigin = strings.TrimSuffix(c.IDEOrigin, "/")
	case "gateway":
		if c.ControlPlaneURL == "" || c.NodeToken == "" {
			return nil, fmt.Errorf("LLMR_CONTROL_PLANE_URL and LLMR_NODE_TOKEN are required in mode=gateway")
		}
	default:
		return nil, fmt.Errorf("unknown mode %q", c.Mode)
	}
	return c, nil
}

// checkSecret refuses the values that ship in examples and anything too short
// to be a secret. An optional secret may be empty (the feature stays off).
func checkSecret(name, v string, required bool, minLen int) error {
	if v == "" {
		if required {
			return fmt.Errorf("%s is required in mode=all (openssl rand -hex 16)", name)
		}
		return nil
	}
	switch strings.ToLower(v) {
	case "change-me", "change-me-too", "changeme", "secret", "password", "admin", "test", "example":
		return fmt.Errorf("%s is set to a placeholder value; generate one with: openssl rand -hex 16", name)
	}
	if len(v) < minLen {
		return fmt.Errorf("%s must be at least %d characters (openssl rand -hex 16)", name, minLen)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
