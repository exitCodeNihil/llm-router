package store

// Postgres advisory lock ids, one per job that must run in one control plane
// at a time. They are session locks, so they need a direct connection: a
// transaction-mode pooler such as PgBouncer releases them at once.
//
// internal/telemetry uses 0x6c6c6d7274656c for its one-time settings move.
const (
	LockMigrate  int64 = 0x6c6c6d726d6967 // "llmrmig": schema migrations
	LockMaintain int64 = 0x6c6c6d726d6e74 // "llmrmnt": partition indexes and retention
	// LockSSOSecret is a transaction lock, for the one-time encryption of the
	// SSO client secret.
	LockSSOSecret int64 = 0x6c6c6d72737363 // "llmrssc"
)
