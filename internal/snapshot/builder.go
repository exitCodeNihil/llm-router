package snapshot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Builder rebuilds snapshots from Postgres (mode=all).
type Builder struct {
	Pool          *pgxpool.Pool
	EncryptionKey string
}

func (b *Builder) Build(ctx context.Context) (*Snapshot, error) {
	s := &Snapshot{
		KeysByHash:         map[[32]byte]*Key{},
		UsersByID:          map[string]*User{},
		UsersByEmail:       map[string]*User{},
		TeamsByID:          map[string]*Team{},
		DeploymentsByModel: map[string][]*Deployment{},
		Prices:             map[string]Price{},
		Spend:              map[string]Spend{},
	}

	if err := b.Pool.QueryRow(ctx,
		`SELECT (value::text)::bigint FROM settings WHERE key='config_version'`).Scan(&s.Version); err != nil {
		return nil, fmt.Errorf("config_version: %w", err)
	}

	rows, err := b.Pool.Query(ctx, `SELECT id, email, role, budget_usd, budget_period, rpm_limit, tpm_limit, tags, allowed_models FROM users WHERE NOT disabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		u := &User{}
		var period *string
		var allowed []string
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.BudgetUSD, &period, &u.RPMLimit, &u.TPMLimit, &u.Tags, &allowed); err != nil {
			return nil, err
		}
		u.AllowedModels = setOf(allowed)
		u.BudgetPeriod = deref(period)
		s.UsersByID[u.ID] = u
		s.UsersByEmail[strings.ToLower(u.Email)] = u
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = b.Pool.Query(ctx, `SELECT id, name, budget_usd, budget_period, rpm_limit, tpm_limit, tags, allowed_models FROM teams`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &Team{}
		var period *string
		var allowed []string
		if err := rows.Scan(&t.ID, &t.Name, &t.BudgetUSD, &period, &t.RPMLimit, &t.TPMLimit, &t.Tags, &allowed); err != nil {
			return nil, err
		}
		t.AllowedModels = setOf(allowed)
		t.BudgetPeriod = deref(period)
		s.TeamsByID[t.ID] = t
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// Memberships, then each user's model access as the union of their
	// teams' policies: what a person may call by default is what their
	// teams may call.
	rows, err = b.Pool.Query(ctx, `SELECT user_id, team_id, budget_usd, budget_period FROM team_members`)
	if err != nil {
		return nil, err
	}
	unrestricted := map[string]bool{}
	for rows.Next() {
		var userID, teamID string
		var budget *float64
		var period *string
		if err := rows.Scan(&userID, &teamID, &budget, &period); err != nil {
			return nil, err
		}
		u := s.UsersByID[userID]
		t := s.TeamsByID[teamID]
		if u == nil || t == nil {
			continue
		}
		u.TeamIDs = append(u.TeamIDs, teamID)
		if u.Memberships == nil {
			u.Memberships = map[string]*Membership{}
		}
		u.Memberships[teamID] = &Membership{BudgetUSD: budget, BudgetPeriod: deref(period)}
		if t.AllowedModels == nil {
			unrestricted[userID] = true
			continue
		}
		if u.TeamModels == nil {
			u.TeamModels = map[string]bool{}
		}
		for name := range t.AllowedModels {
			u.TeamModels[name] = true
		}
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	for id := range unrestricted {
		s.UsersByID[id].TeamModels = nil
	}

	rows, err = b.Pool.Query(ctx, `
		SELECT id, key_hash, key_prefix, name, user_id, team_id, allowed_models,
		       budget_usd, budget_period, rpm_limit, tpm_limit, expires_at, tags
		FROM api_keys WHERE NOT disabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		k := &Key{}
		var hash []byte
		var userID, teamID, period *string
		var allowed []string
		if err := rows.Scan(&k.ID, &hash, &k.Prefix, &k.Name, &userID, &teamID, &allowed,
			&k.BudgetUSD, &period, &k.RPMLimit, &k.TPMLimit, &k.ExpiresAt, &k.Tags); err != nil {
			return nil, err
		}
		k.UserID, k.TeamID, k.BudgetPeriod = deref(userID), deref(teamID), deref(period)
		if allowed != nil {
			k.AllowedModels = map[string]bool{}
			for _, m := range allowed {
				k.AllowedModels[m] = true
			}
		}
		if len(hash) == sha256.Size {
			var h [32]byte
			copy(h[:], hash)
			s.KeysByHash[h] = k
		}
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	providers := map[string]*Provider{}
	rows, err = b.Pool.Query(ctx, `SELECT id, name, type, base_url, auth_mode, api_key_enc, config FROM providers`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p := &Provider{}
		var enc []byte
		var cfg map[string]any
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthMode, &enc, &cfg); err != nil {
			return nil, err
		}
		// only decrypt when the auth mode actually uses the stored key —
		// entra/none providers may carry stale ciphertext from an old key
		if len(enc) > 0 && ProviderHasSecret(p.AuthMode) {
			key, err := Decrypt(b.EncryptionKey, enc)
			if err != nil {
				slog.Error("decrypt provider api key failed; provider disabled", "provider", p.Name, "err", err)
				continue
			}
			p.APIKey = key
		}
		if v, ok := cfg["api_version"].(string); ok {
			p.APIVersion = v
		}
		if v, ok := cfg["project"].(string); ok {
			p.Project = v
		}
		if v, ok := cfg["location"].(string); ok {
			p.Location = v
		}
		providers[p.ID] = p
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = b.Pool.Query(ctx, `
		SELECT id, provider_id, model_name, upstream_name, api_flavor, priority, catalog_model_id, input_per_1m, output_per_1m, cached_input_per_1m
		FROM model_deployments WHERE enabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		d := &Deployment{}
		var providerID string
		var catalogID *string
		if err := rows.Scan(&d.ID, &providerID, &d.ModelName, &d.UpstreamName, &d.APIFlavor, &d.Priority,
			&catalogID, &d.InputPer1M, &d.OutputPer1M, &d.CachedInputPer1M); err != nil {
			return nil, err
		}
		d.CatalogModelID = deref(catalogID)
		d.Provider = providers[providerID]
		if d.Provider == nil {
			continue
		}
		s.DeploymentsByModel[d.ModelName] = append(s.DeploymentsByModel[d.ModelName], d)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	rows, err = b.Pool.Query(ctx, `SELECT model_id, input_per_1m, output_per_1m, cached_input_per_1m FROM price_catalog`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p Price
		if err := rows.Scan(&id, &p.InputPer1M, &p.OutputPer1M, &p.CachedInputPer1M); err != nil {
			return nil, err
		}
		s.Prices[id] = p
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	// After prices: the routing order within a priority is by list price.
	s.SortDeployments()

	rows, err = b.Pool.Query(ctx, `SELECT id, type, issuer_url, audience, claim_mapping FROM token_issuers WHERE enabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		iss := &Issuer{}
		var mapping map[string]any
		if err := rows.Scan(&iss.ID, &iss.Type, &iss.IssuerURL, &iss.Audience, &mapping); err != nil {
			return nil, err
		}
		applyClaimMapping(iss, mapping)
		s.Issuers = append(s.Issuers, iss)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	var telemetryRaw []byte
	if err := b.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='telemetry'`).Scan(&telemetryRaw); err == nil {
		if err := json.Unmarshal(telemetryRaw, &s.Telemetry); err != nil {
			slog.Error("corrupt telemetry settings; exporter disabled", "err", err)
		}
	}

	if err := b.loadSpend(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// loadSpend caches current-period spend per scope for O(1) budget checks.
// Total-period budgets sum all rows; daily/monthly sum from the period start.
func (b *Builder) loadSpend(ctx context.Context, s *Snapshot) error {
	rows, err := b.Pool.Query(ctx,
		`SELECT scope_type, scope_id, period_start, usd FROM spend_counters`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		scopeType, scopeID string
		periodStart        time.Time
		usd                float64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.scopeType, &r.scopeID, &r.periodStart, &r.usd); err != nil {
			return err
		}
		all = append(all, r)
	}
	if rows.Err() != nil {
		return rows.Err()
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	keysByID := make(map[string]*Key, len(s.KeysByHash))
	for _, k := range s.KeysByHash {
		keysByID[k.ID] = k
	}
	period := func(scopeType, scopeID string) string {
		switch scopeType {
		case "key":
			if k := keysByID[scopeID]; k != nil {
				return k.BudgetPeriod
			}
		case "user":
			if u := s.UsersByID[scopeID]; u != nil {
				return u.BudgetPeriod
			}
		case "team":
			if t := s.TeamsByID[scopeID]; t != nil {
				return t.BudgetPeriod
			}
		case "member":
			if userID, teamID, ok := strings.Cut(scopeID, ":"); ok {
				if u := s.UsersByID[userID]; u != nil {
					if mem := u.Memberships[teamID]; mem != nil {
						return mem.BudgetPeriod
					}
				}
			}
		}
		return ""
	}

	for _, r := range all {
		p := period(r.scopeType, r.scopeID)
		include := false
		switch p {
		case "daily":
			include = !r.periodStart.Before(today)
		case "monthly":
			include = !r.periodStart.Before(monthStart)
		case "total", "":
			// "" = scope with no period set (or deleted scope): count everything,
			// so a budget_usd without a period behaves as a total budget
			include = true
		}
		if include {
			key := r.scopeType + ":" + r.scopeID
			sp := s.Spend[key]
			sp.USD += r.usd
			s.Spend[key] = sp
		}
	}
	return nil
}

func applyClaimMapping(iss *Issuer, m map[string]any) {
	iss.EmailClaim = "email"
	if v, ok := m["email_claim"].(string); ok && v != "" {
		iss.EmailClaim = v
	}
	if v, ok := m["match"].(map[string]any); ok {
		iss.Match = map[string]string{}
		for k, val := range v {
			if sv, ok := val.(string); ok {
				iss.Match[k] = sv
			}
		}
	}
	if v, ok := m["map_to_team"].(string); ok {
		iss.MapToTeam = v
	}
	if v, ok := m["auto_create_user"].(bool); ok {
		iss.AutoCreateUser = v
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Encrypt/Decrypt protect provider API keys at rest (AES-256-GCM, key from env).
func Encrypt(passphrase, plaintext string) ([]byte, error) {
	gcm, err := newGCM(passphrase)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func Decrypt(passphrase string, data []byte) (string, error) {
	gcm, err := newGCM(passphrase)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	pt, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func newGCM(passphrase string) (cipher.AEAD, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("LLMR_ENCRYPTION_KEY is not set")
	}
	sum := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Refresher rebuilds the snapshot on Postgres NOTIFY or every pollInterval.
func Refresher(ctx context.Context, pool *pgxpool.Pool, b *Builder, h *Holder, pollInterval time.Duration) {
	notify := make(chan struct{}, 1)
	go listen(ctx, pool, notify)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-ticker.C:
		}
		s, err := b.Build(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("snapshot rebuild failed", "err", err)
			}
			continue
		}
		// always swap: spend counters change without config version bumps
		h.Set(s)
	}
}

func listen(ctx context.Context, pool *pgxpool.Pool, notify chan<- struct{}) {
	for ctx.Err() == nil {
		if err := listenOnce(ctx, pool, notify); err != nil && ctx.Err() == nil {
			slog.Warn("config LISTEN dropped; reconnecting", "err", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, notify chan<- struct{}) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN llmr_config`); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case notify <- struct{}{}:
		default:
		}
	}
}

// ProviderHasSecret reports whether api_key_enc holds something this auth mode
// actually uses. entra/adc/none providers may carry stale ciphertext from an
// earlier mode, and decrypting it would fail the whole provider for nothing.
func ProviderHasSecret(authMode string) bool {
	switch authMode {
	case "api_key", "bearer", "gcp_sa":
		return true
	}
	return false
}

// setOf turns a stored model list into a lookup; a NULL column stays nil,
// which every check reads as "no restriction".
func setOf(names []string) map[string]bool {
	if names == nil {
		return nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
