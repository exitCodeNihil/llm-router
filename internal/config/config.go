// Package config holds process configuration read from flags and environment.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen        string // address for the public gateway + console + mgmt API
	DatabaseURL   string
	AdminToken    string // bootstrap admin token for mgmt API
	EncryptionKey string // hex/plain 32-byte key for provider secret encryption

	// IDEListen/IDEOrigin put the workspace IDE proxy on its own origin so
	// content rendered by a container is never same-origin with the console.
	// Both empty = the IDE is served from the main listener (not recommended).
	IDEListen string
	IDEOrigin string
	// CookieDomain scopes the session cookie (e.g. ".example.com") so an IDE
	// origin on a sibling hostname receives it. Empty = host-only cookie,
	// which is right when the IDE differs from the console by port alone.
	CookieDomain string
	// TrustedProxies lists the proxies (addresses or CIDR ranges) whose
	// X-Forwarded-For header names the client, for sign-in rate limits.
	TrustedProxies []netip.Prefix
	// MetricsListen serves /metrics on its own listener instead of the main
	// one ("off": not at all). Empty = on the main listener, like a gateway.
	MetricsListen string

	// DBMaxConns and DBConsoleMaxConns size the two Postgres pools: ingest
	// and sync, and console reads. DBConsoleTimeout is the statement timeout
	// on console reads. Zero = the store's default.
	DBMaxConns        int
	DBConsoleMaxConns int
	DBConsoleTimeout  time.Duration

	// UsageRetentionMonths and RollupRetentionMonths are how many whole months
	// of per-request usage, and of daily rollups and per-day spend, are kept.
	// Zero keeps them for ever.
	UsageRetentionMonths  int
	RollupRetentionMonths int
	// AuditRetentionMonths is how many months of the audit log are kept.
	// Zero keeps it for ever.
	AuditRetentionMonths int

	// LogLevel is the least severe level logged; LogJSON writes one JSON
	// object per line instead of text.
	LogLevel slog.Level
	LogJSON  bool
}

func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("llmrouter", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", envOr("LLMR_LISTEN", ":8080"), "listen address")
	fs.StringVar(&c.DatabaseURL, "database-url", os.Getenv("LLMR_DATABASE_URL"), "postgres URL")
	fs.StringVar(&c.IDEListen, "ide-listen", os.Getenv("LLMR_IDE_LISTEN"), "listen address for the workspace IDE proxy")
	fs.StringVar(&c.IDEOrigin, "ide-origin", os.Getenv("LLMR_IDE_ORIGIN"), "public origin of the IDE listener, e.g. http://localhost:8081")
	fs.StringVar(&c.CookieDomain, "cookie-domain", os.Getenv("LLMR_COOKIE_DOMAIN"), "session cookie Domain attribute, e.g. .example.com (needed when the IDE origin is a different hostname)")
	fs.StringVar(&c.MetricsListen, "metrics-listen", os.Getenv("LLMR_METRICS_LISTEN"), "serve /metrics on this address instead of the main listener (off: not at all)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.AdminToken = os.Getenv("LLMR_ADMIN_TOKEN")
	trusted, err := parsePrefixes(os.Getenv("LLMR_TRUSTED_PROXIES"))
	if err != nil {
		return nil, fmt.Errorf("LLMR_TRUSTED_PROXIES: %w", err)
	}
	c.TrustedProxies = trusted
	c.EncryptionKey = os.Getenv("LLMR_ENCRYPTION_KEY")
	if c.DBMaxConns, err = envInt("LLMR_DB_MAX_CONNS"); err != nil {
		return nil, err
	}
	if c.DBConsoleMaxConns, err = envInt("LLMR_DB_CONSOLE_MAX_CONNS"); err != nil {
		return nil, err
	}
	switch v := strings.ToLower(envOr("LLMR_LOG_LEVEL", "info")); v {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("LLMR_LOG_LEVEL must be debug, info, warn or error")
	}
	switch v := strings.ToLower(envOr("LLMR_LOG_FORMAT", "text")); v {
	case "text":
	case "json":
		c.LogJSON = true
	default:
		return nil, fmt.Errorf("LLMR_LOG_FORMAT must be text or json")
	}
	if c.UsageRetentionMonths, err = envInt("LLMR_USAGE_RETENTION_MONTHS"); err != nil {
		return nil, err
	}
	if c.RollupRetentionMonths, err = envInt("LLMR_ROLLUP_RETENTION_MONTHS"); err != nil {
		return nil, err
	}
	if c.AuditRetentionMonths, err = envInt("LLMR_AUDIT_RETENTION_MONTHS"); err != nil {
		return nil, err
	}
	if v := os.Getenv("LLMR_DB_CONSOLE_TIMEOUT"); v != "" {
		if c.DBConsoleTimeout, err = time.ParseDuration(v); err != nil || c.DBConsoleTimeout <= 0 {
			return nil, fmt.Errorf("LLMR_DB_CONSOLE_TIMEOUT must be a duration like 30s")
		}
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("LLMR_DATABASE_URL is required")
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
	return c, nil
}

// checkSecret refuses the values that ship in examples and anything too short
// to be a secret. An optional secret may be empty (the feature stays off).
func checkSecret(name, v string, required bool, minLen int) error {
	if v == "" {
		if required {
			return fmt.Errorf("%s is required (openssl rand -hex 16)", name)
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

// envInt reads a whole number, zero or more; unset is 0.
func envInt(key string) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a whole number, 0 or more", key)
	}
	return n, nil
}

// parsePrefixes reads a comma-separated list of addresses or CIDR ranges.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, err
			}
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
