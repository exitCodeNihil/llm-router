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

// Builder rebuilds snapshots from Postgres.
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
		GatewayGroups:      map[string]*GatewayGroup{},
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
		p.ApplyConfig(cfg)
		providers[p.ID] = p
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = b.Pool.Query(ctx, `
		SELECT id, provider_id, model_name, upstream_name, api_flavor, priority, catalog_model_id, input_per_1m, output_per_1m, cached_input_per_1m,
		       region, timeout_ms
		FROM model_deployments WHERE enabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		d := &Deployment{}
		var providerID string
		var catalogID *string
		if err := rows.Scan(&d.ID, &providerID, &d.ModelName, &d.UpstreamName, &d.APIFlavor, &d.Priority,
			&catalogID, &d.InputPer1M, &d.OutputPer1M, &d.CachedInputPer1M, &d.Region, &d.TimeoutMS); err != nil {
			return nil, err
		}
		d.CatalogModelID = deref(catalogID)
		d.Provider = providers[providerID]
		if d.Provider == nil {
			continue
		}
		if d.Provider.Type == "anthropic" {
			d.APIFlavor = "anthropic" // whatever the model is called
		}
		s.DeploymentsByModel[d.ModelName] = append(s.DeploymentsByModel[d.ModelName], d)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	rows, err = b.Pool.Query(ctx, `
		SELECT model_id, input_per_1m, output_per_1m, cached_input_per_1m,
		       coalesce(long_prompt_tokens, 0), coalesce(long_prompt_multiplier, 1)
		FROM price_catalog`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p Price
		if err := rows.Scan(&id, &p.InputPer1M, &p.OutputPer1M, &p.CachedInputPer1M, &p.LongPromptTokens, &p.LongPromptMultiplier); err != nil {
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
		s.Telemetry = parseTelemetry(telemetryRaw)
	}

	if err := b.loadDestinations(ctx, s); err != nil {
		return nil, err
	}

	rows, err = b.Pool.Query(ctx, `SELECT id, name, region, allowed_teams, data_rule FROM gateway_groups`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		g := &GatewayGroup{}
		var teams []string
		if err := rows.Scan(&g.ID, &g.Name, &g.Region, &teams, &g.DataRule); err != nil {
			return nil, err
		}
		if len(teams) > 0 {
			g.AllowedTeams = map[string]bool{}
			for _, t := range teams {
				g.AllowedTeams[t] = true
			}
		}
		s.GatewayGroups[g.ID] = g
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	if err := b.loadSpend(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// parseTelemetry reads the capture policy. One that can't be read exports
// nothing: the zero policy would export everything.
func parseTelemetry(raw []byte) Telemetry {
	var t Telemetry
	if err := json.Unmarshal(raw, &t); err != nil {
		slog.Error("corrupt telemetry settings; export is off until they are saved again", "err", err)
		return Telemetry{Mode: "off"}
	}
	return t
}

// loadDestinations reads the telemetry destinations. One whose secrets can't
// be decrypted is kept with Err set, so the console can say why it is idle.
func (b *Builder) loadDestinations(ctx context.Context, s *Snapshot) error {
	rows, err := b.Pool.Query(ctx, `
		SELECT id, name, kind, enabled, endpoint, traces_url, config, secrets_enc, content_mode,
		       max_content_bytes, team_scope, team_values, group_scope, group_values
		FROM telemetry_destinations ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		d := &Destination{}
		var enc []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.Enabled, &d.Endpoint, &d.TracesURL, &d.Config, &enc,
			&d.ContentMode, &d.MaxContentBytes, &d.Teams.Mode, &d.Teams.Values, &d.Groups.Mode, &d.Groups.Values); err != nil {
			return err
		}
		if len(enc) > 0 {
			plain, err := Decrypt(b.EncryptionKey, enc)
			if err == nil {
				err = json.Unmarshal([]byte(plain), &d.Secrets)
			}
			if err != nil {
				slog.Error("decrypt telemetry destination secrets failed; destination idle", "destination", d.Name, "err", err)
				d.Err = "secrets can't be decrypted with LLMR_ENCRYPTION_KEY: " + err.Error()
				d.Secrets = nil
			}
		}
		s.Destinations = append(s.Destinations, d)
	}
	return rows.Err()
}

// spendTotalStart is the period_start of a scope's all-time row in
// spend_totals (usage.TotalStart).
var spendTotalStart = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// loadSpend caches current-period spend for every scope that has a budget, for
// O(1) budget checks. It reads one row per scope: today's from spend_counters,
// this month's and the all-time one from spend_totals.
func (b *Builder) loadSpend(ctx context.Context, s *Snapshot) error {
	// The budgeted scopes, by the period their budget runs over.
	type scopes struct{ types, ids []string }
	var daily, monthly, total scopes
	add := func(scopeType, scopeID string, budget *float64, period string) {
		if budget == nil {
			return
		}
		var to *scopes
		switch period {
		case "daily":
			to = &daily
		case "monthly":
			to = &monthly
		case "total", "":
			// a budget_usd without a period behaves as a total budget
			to = &total
		default:
			return
		}
		to.types, to.ids = append(to.types, scopeType), append(to.ids, scopeID)
	}
	for _, k := range s.KeysByHash {
		add("key", k.ID, k.BudgetUSD, k.BudgetPeriod)
	}
	for _, u := range s.UsersByID {
		add("user", u.ID, u.BudgetUSD, u.BudgetPeriod)
		for teamID, m := range u.Memberships {
			add("member", u.ID+":"+teamID, m.BudgetUSD, m.BudgetPeriod)
		}
	}
	for _, t := range s.TeamsByID {
		add("team", t.ID, t.BudgetUSD, t.BudgetPeriod)
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	load := func(w scopes, sql string, args ...any) error {
		if len(w.ids) == 0 {
			return nil
		}
		rows, err := b.Pool.Query(ctx, sql, append([]any{w.types, w.ids}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var scopeType, scopeID string
			var usd float64
			if err := rows.Scan(&scopeType, &scopeID, &usd); err != nil {
				return err
			}
			s.Spend[scopeType+":"+scopeID] = Spend{USD: usd}
		}
		return rows.Err()
	}
	const fromTotals = `SELECT c.scope_type, c.scope_id, c.usd
		FROM unnest($1::text[], $2::text[]) AS w(scope_type, scope_id)
		JOIN spend_totals c ON c.period = $3 AND c.period_start = $4
		 AND c.scope_type = w.scope_type AND c.scope_id = w.scope_id`
	if err := load(daily, `SELECT c.scope_type, c.scope_id, c.usd
		FROM unnest($1::text[], $2::text[]) AS w(scope_type, scope_id)
		JOIN spend_counters c ON c.scope_type = w.scope_type AND c.scope_id = w.scope_id
		 AND c.period_start = $3`, today); err != nil {
		return err
	}
	if err := load(monthly, fromTotals, "month", monthStart); err != nil {
		return err
	}
	return load(total, fromTotals, "total", spendTotalStart)
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
		start := time.Now()
		s, err := b.Build(ctx)
		if err != nil {
			if ctx.Err() == nil {
				h.Failed()
				slog.Error("snapshot rebuild failed", "err", err)
			}
			continue
		}
		h.Built(time.Since(start))
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
