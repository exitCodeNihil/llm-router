export type BudgetPeriod = "daily" | "monthly" | "total";

export interface Me {
  user_id: string;
  email: string;
  is_admin: boolean;
  team_roles: Record<string, "admin" | "member">;
  /** Workspaces are enabled and this account is allowed to use them. */
  workspaces: boolean;
  /** False for SSO-provisioned users who have not set a password yet. */
  has_password: boolean;
  /** Origin the IDE iframe loads from; "" when it shares this origin. */
  ide_origin: string;
}

export interface User {
  id: string;
  email: string;
  name: string;
  role: string;
  /** Model policy: null = every model; a list is all this user's keys may call. */
  allowed_models?: string[] | null;
  budget_usd: number | null;
  budget_period: BudgetPeriod | null;
  rpm_limit: number | null;
  tpm_limit: number | null;
  disabled: boolean;
  tags: string[] | null;
  created_at: string;
}

export interface Team {
  id: string;
  name: string;
  /** Model policy: null = every model; a list is all this team's keys may call. */
  allowed_models?: string[] | null;
  budget_usd: number | null;
  budget_period: BudgetPeriod | null;
  rpm_limit: number | null;
  tpm_limit: number | null;
  tags: string[] | null;
  created_at: string;
}

export interface TeamMember {
  user_id: string;
  email: string;
  name: string;
  role: string;
  /** This member's share of the team budget; null = no per-member cap. */
  budget_usd: number | null;
  budget_period: BudgetPeriod | null;
}

export interface ApiKey {
  id: string;
  key_prefix: string;
  name: string;
  user_id: string | null;
  team_id: string | null;
  allowed_models: string[] | null;
  budget_usd: number | null;
  budget_period: BudgetPeriod | null;
  rpm_limit: number | null;
  tpm_limit: number | null;
  expires_at: string | null;
  disabled: boolean;
  tags: string[] | null;
  created_at: string;
}

export type ProviderType = "azure" | "openai_compatible" | "gcp_vertex" | "openrouter";

export interface Provider {
  id: string;
  name: string;
  type: ProviderType;
  base_url: string;
  auth_mode: string;
  has_api_key: boolean;
  config: Record<string, unknown> | null;
  created_at: string;
}

export interface Deployment {
  id: string;
  provider_id: string;
  provider_name: string;
  model_name: string;
  upstream_name: string;
  priority: number;
  catalog_model_id: string | null;
  /** Optional, operator-set context window in tokens; informational only. */
  context_tokens: number | null;
  input_per_1m: number | null;
  output_per_1m: number | null;
  /** Optional cache-read rate; unset bills cache reads at input_per_1m. */
  cached_input_per_1m: number | null;
  enabled: boolean;
  created_at: string;
}

export interface WorkspaceTemplate {
  id: string;
  name: string;
  description: string;
  image: string;
  dockerfile: string;
  source: "seed" | "admin";
  updated_at: string;
}

export interface WorkspaceUserFile {
  path: string;
  mode: number;
  size: number;
  updated_at: string;
}

/** A model as a member sees it: callable name, list price, health. */
export interface AvailableModel {
  name: string;
  input_per_1m: number | null;
  output_per_1m: number | null;
  backends: number;
  via: string[] | null;
  api_flavor: string;
  passthrough: boolean;
  status: "ok" | "degraded" | "down";
}

export interface Workspace {
  id: string;
  user_id: string;
  name: string;
  image: string;
  template_id?: string | null;
  container_id: string;
  status: "stopped" | "running" | "error";
  created_at: string;
  /** Present for admins, who see every workspace. */
  owner_email?: string;
}

export interface WorkspaceScope {
  scope_type: "team" | "user";
  scope_value: string;
}

export interface WorkspaceSettings {
  enabled: boolean;
  /** "docker" (podman serves the same API) or "kubernetes". */
  runtime: "docker" | "kubernetes";
  /** Container runtime socket; moves per host, so it is never hardcoded. */
  socket: string;
  /** Kubernetes only: PVC storage class (blank = cluster default) and size. */
  storage_class: string;
  disk_gb: number;
  /** How the workspace reaches the gateway from inside the container. */
  gateway_url: string;
  default_image: string;
  /** Images non-admins may pick from; empty means default_image only. */
  images: string[];
  memory_mb: number;
  cpus: number;
  /** Per-workspace daily spend cap on the agent's key. */
  budget_usd: number;
  max_per_user: number;
  /** Empty means admins only — never everyone. */
  allow: WorkspaceScope[];
}

/** One entry in a workspace directory listing. */
export interface WorkspaceEntry {
  name: string;
  dir: boolean;
}

export interface Price {
  model_id: string;
  input_per_1m: number;
  output_per_1m: number;
  cached_input_per_1m: number | null;
  source: "seed" | "admin";
  updated_at: string;
}

export interface SsoSettings {
  configured: boolean;
  sso?: {
    issuer_url: string;
    client_id: string;
    client_secret: string;
    redirect_url: string;
    auto_create_users: boolean;
  };
}

export interface ClaimMapping {
  email_claim?: string;
  match?: Record<string, string>;
  map_to_team?: string;
  auto_create_user?: boolean;
}

export interface TokenIssuer {
  id: string;
  type: "entra" | "gcp" | "oidc";
  issuer_url: string;
  audience: string;
  claim_mapping: ClaimMapping | null;
  enabled: boolean;
  created_at: string;
}

export interface EdgeNode {
  id: string;
  name: string;
  last_seen_at: string | null;
  last_seen_version: string | null;
  created_at: string;
}

export type TelemetryMode = "everything" | "by_rule" | "off";
export type RuleScope = "team" | "user" | "key" | "tag";
export type Capture = "content" | "meta";

export interface TelemetryRule {
  id: string;
  order: number;
  scope_type: RuleScope;
  scope_value: string; // team/user/key id, or the tag string
  capture: Capture;
  sample: number; // 0..100
  enabled: boolean;
}

// Delivery outcome from the in-process exporter. Distinct from the live feed,
// which lists events *selected* for export: a rejected key still fills the feed.
export interface TelemetryHealth {
  delivered: number;
  dropped: number;
  last_ok_at?: string;
  last_error?: string;
  last_error_at?: string;
}

export interface TelemetrySettings {
  configured: boolean;
  health?: TelemetryHealth;
  /** Langfuse trace URL prefix; append a request_id. Absent when export is off. */
  trace_base_url?: string;
  telemetry?: {
    type: "langfuse";
    host: string;
    public_key: string;
    secret_key: string;
    enabled: boolean;
    capture_content: boolean;
    mode: TelemetryMode | ""; // "" == everything
    rules: TelemetryRule[];
    // legacy scope fields — removed on the backend, kept optional so the old
    // Telemetry.tsx card keeps compiling until the Observability page lands.
    scope?: "all" | "tags" | "teams";
    scope_tags?: string[];
    scope_team_ids?: string[];
  };
}

// ── Live metrics (GET /api/metrics/live, admin) ──────────────────
export interface LiveProvider {
  id: string;
  name: string;
  p50_ms: number;
  p95_ms: number;
  err_pct: number;
  rpm: number;
  share_pct: number;
  led: "ok" | "warn" | "err";
}
export interface LiveEdge {
  key_id: string;
  key_name: string;
  model: string;
  provider_id: string;
  provider_name: string;
  volume: number;
}
export interface LiveNode {
  id: string;
  name: string;
  rps: number;
}
export interface LiveTotals {
  rpm: number;
  keys: number;
  aliases: number;
  upstreams: number;
}
export interface LiveMetrics {
  providers: LiveProvider[];
  edges: LiveEdge[];
  nodes: LiveNode[];
  totals: LiveTotals;
}

// ── Routing flow over a time range (GET /api/usage/flow) ─────────
// Volumes come from usage_events so the diagram follows the dashboard's range;
// live metrics only ever hold a rolling minute.
export interface FlowEdgeRow {
  api_key_id: string;
  key_name: string;
  model_name: string;
  provider_id: string;
  provider_name: string;
  volume: number;
}

// ── Spend by team (GET /api/usage/by-team) ───────────────────────
export interface SpendByTeam {
  team_id: string;
  team_name: string;
  usd: number;
  tokens: number;
}

// ── Enriched requests feed (GET /api/usage/requests) ─────────────
export interface RequestRow {
  id: number;
  ts: string;
  request_id: string;
  model_name: string;
  api_key_id: string | null;
  user_id: string | null;
  team_id: string | null;
  deployment_id: string | null;
  provider_id: string | null;
  key_name: string | null;
  provider_name: string | null;
  upstream_name: string | null;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
  latency_ms: number;
  status_code: number;
  stream: boolean;
  /** 1 normally; higher means failover fired. Null on rows predating 0008. */
  attempts: number | null;
  /** The gateway's own reason for a failure, where status_code cannot say. */
  error_code: string | null;
  ttft_ms: number | null;
  /** List value when the request itself was free (subscription pass-through). */
  notional_cost_usd: number | null;
}
export interface RequestFilters {
  limit?: number;
  key_id?: string;
  user_id?: string;
  model?: string;
  status?: "ok" | "err";
  tag?: string;
  since?: string;
  until?: string;
  // Accepted by the shared server-side filter; the Requests page has no control
  // for these yet but forwards them so an analytics drill-down stays faithful.
  team_id?: string;
  provider_id?: string;
  deployment_id?: string;
  error_code?: string;
  request_id?: string;
  priced?: string;
  stream?: string;
  failover?: string;
  days?: string;
}

// ── Deployment req counts (GET /api/usage/by-deployment) ─────────
export interface DeploymentReq {
  deployment_id: string;
  requests: number;
}

// ── Observability ────────────────────────────────────────────────
export interface ObservabilityTest {
  ok: boolean;
  detail: string;
}
export interface ExportedEvent {
  ts: string;
  key_name: string;
  model: string;
  upstream_name: string;
  provider_name: string;
  latency_ms: number;
  cost_usd: number;
  rule_id: string;
  capture: "content" | "meta";
}
export interface RuleStat {
  rule_id: string;
  scope_type: string;
  scope_value: string;
  enabled: boolean;
  matching_rpm: number;
}
export interface RuleStats {
  rules: RuleStat[];
  total_rpm?: number;
  preview?: {
    scope_type: string;
    scope_value: string;
    matching_rpm: number;
  };
}

// ── Nodes ops (GET /api/nodes, admin) ────────────────────────────
export interface Node {
  id: string;
  name: string;
  addr: string;
  role: "leader" | "";
  version: string;
  version_skew: boolean;
  uptime_s: number;
  cpu_pct: number;
  mem_pct: number;
  rps: number;
  p50_ms: number;
  last_seen: string | null;
  status: "ok" | "warn" | "err";
}
export interface ClusterEvent {
  ts: string;
  text: string;
  level: "info" | "warn" | "err";
}
export interface NodesResponse {
  nodes: Node[];
  cluster_events: ClusterEvent[];
}

export interface UsageSummary {
  requests: number;
  tokens: number;
  prompt_tokens: number;
  /** Prompt tokens served from the upstream's prompt cache. */
  cached_tokens: number;
  cost_usd: number;
  unpriced_requests: number;
  errors: number;
}

export interface UsageDaily {
  day: string;
  requests: number;
  tokens: number;
  cost_usd: number;
}

export interface UsageByModel {
  model_name: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
}

export interface UsageRecent {
  ts: string;
  request_id: string;
  model_name: string;
  api_key_id: string | null;
  user_id: string | null;
  team_id: string | null;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
  latency_ms: number;
  status_code: number;
  stream: boolean;
}

// ── Chat playground ─────────────────────────────────────────────
export type ChatContentPart =
  | { type: "text"; text: string }
  | { type: "image_url"; image_url: { url: string } };

export interface ChatToolCall {
  id: string;
  type: "function";
  function: { name: string; arguments: string };
}

/** OpenAI-wire message plus client-only fields (underscore-prefixed; stripped
 *  before sending). */
export interface ChatMessage {
  role: "system" | "user" | "assistant" | "tool";
  content: string | ChatContentPart[] | null;
  tool_calls?: ChatToolCall[];
  tool_call_id?: string;
  _usage?: { prompt: number; completion: number; cached?: number };
  _reasoning?: string;
  _error?: string;
  /** Model that produced an assistant turn; the picker may move on afterwards. */
  _model?: string;
  /**
   * Client-only per-turn telemetry, shown under the assistant message. Stripped
   * before the transcript is sent upstream (see wireMessages).
   */
  _turn?: TurnMetrics;
}

/** What the gateway did for one turn: routing, timing and cost. */
export interface TurnMetrics {
  /** X-Request-Id — correlates with the row on the Requests page. */
  requestId?: string;
  /** X-Llmr-Provider — which provider actually served the turn. */
  provider?: string;
  /** X-Llmr-Upstream — the upstream model name behind the alias. */
  upstream?: string;
  /** X-Llmr-Attempts — >1 means failover was used. */
  attempts?: number;
  /** Time to first token, in ms: what the caller perceives as latency. */
  ttftMs?: number;
  /** Total wall-clock time for the turn, in ms. */
  totalMs?: number;
  /** Estimated cost in USD, computed from the same rules the gateway bills by. */
  costUsd?: number;
  /** True when no price applies, so the gateway records the request unpriced. */
  unpriced?: boolean;
  /** Which API key the turn was sent as, when not the console session. */
  sentAsKey?: string;
}

export interface ChatUITool {
  name: string;
  description: string;
  parameters: string; // JSON text, validated in the editor
}

export interface ChatSettings {
  temperature: number | null;
  max_tokens: number | null;
  response_format: "none" | "json_object" | "json_schema";
  json_schema: string; // JSON text
  tools: ChatUITool[];
  tool_choice: "auto" | "none" | "required";
}

export interface ChatSummary {
  id: string;
  title: string;
  model: string;
  updated_at: string;
}

export interface ChatDetail extends ChatSummary {
  system_prompt: string;
  settings: Partial<ChatSettings> | null;
  messages: ChatMessage[] | null;
  created_at: string;
}

// ── Analytics (GET /api/usage/{stats,breakdown,timeseries}) ──────
/** Filter state shared by the analytics page and its drill-down into Requests. */
export interface UsageQuery {
  days: number;
  model?: string;
  key_id?: string;
  team_id?: string;
  provider_id?: string;
  error_code?: string;
  status?: "" | "ok" | "err";
  priced?: "" | "priced" | "unpriced";
  stream?: "" | "true" | "false";
  failover?: "" | "true";
  tag?: string;
}

export interface UsageStats {
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  /** List value of traffic that cost nothing (subscription). Never spend. */
  notional_cost_usd: number;
  unpriced_requests: number;
  errors: number;
  failovers: number;
  models: number;
  p50_ms: number;
  p95_ms: number;
  ttft_p50_ms: number;
}

export type BreakdownDim =
  | "model"
  | "provider"
  | "deployment"
  | "key"
  | "user"
  | "team"
  | "status"
  | "error_code"
  | "edge_node";

export interface BreakdownRow {
  bucket: string;
  label: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  notional_cost_usd: number;
  errors: number;
  failovers: number;
  p50_ms: number;
  p95_ms: number;
  ttft_p50_ms: number;
}

export interface TimeseriesPoint {
  bucket: string;
  requests: number;
  tokens: number;
  cost_usd: number;
  errors: number;
}
