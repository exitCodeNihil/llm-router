import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type QueryKey,
} from "@tanstack/react-query";
import { api, upload, num } from "./api";
import type {
  AvailableModel,
  ApiKey,
  BreakdownDim,
  BreakdownRow,
  ChatDetail,
  ChatSummary,
  Deployment,
  DeploymentReq,
  EdgeNode,
  ExportedEvent,
  FlowEdgeRow,
  LiveMetrics,
  Me,
  Node,
  NodesResponse,
  ObservabilityTest,
  Price,
  Provider,
  RequestFilters,
  RequestRow,
  RuleStats,
  SpendByTeam,
  SsoSettings,
  Team,
  TeamMember,
  TelemetrySettings,
  TimeseriesPoint,
  TokenIssuer,
  UsageByModel,
  UsageDaily,
  UsageQuery,
  UsageRecent,
  UsageStats,
  UsageSummary,
  User,
  Workspace,
  WorkspaceEntry,
  WorkspaceSettings,
  WorkspaceTemplate,
  WorkspaceUserFile,
} from "./types";

// ── Identity ────────────────────────────────────────────────────
export function useMe() {
  return useQuery({
    queryKey: ["me"],
    queryFn: () => api.get<Me>("/api/me"),
    retry: false,
    staleTime: 60_000,
  });
}

// ── Generic mutation helper: run, then invalidate query keys ─────
function useInvalidatingMutation<V>(
  fn: (v: V) => Promise<unknown>,
  invalidate: QueryKey[],
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () =>
      invalidate.forEach((k) => qc.invalidateQueries({ queryKey: k })),
  });
}

// ── Users ───────────────────────────────────────────────────────
export function useUsers(enabled = true) {
  return useQuery({
    queryKey: ["users"],
    queryFn: async () =>
      (await api.get<User[]>("/api/users")).map((u) => ({
        ...u,
        budget_usd: u.budget_usd == null ? null : num(u.budget_usd),
        rpm_limit: u.rpm_limit == null ? null : num(u.rpm_limit),
        tpm_limit: u.tpm_limit == null ? null : num(u.tpm_limit),
      })),
    enabled,
  });
}
export const useCreateUser = () =>
  useInvalidatingMutation(
    (b: Partial<User>) => api.post("/api/users", b),
    [["users"]],
  );
export const useUpdateUser = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Partial<User>) =>
      api.patch(`/api/users/${id}`, b),
    [["users"]],
  );
export const useDeleteUser = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/users/${id}`),
    [["users"]],
  );
export const useSetUserPassword = () =>
  useMutation({
    mutationFn: ({ id, password }: { id: string; password: string }) =>
      api.post(`/api/users/${id}/password`, { password }),
  });

// ── Teams ───────────────────────────────────────────────────────
export function useTeams() {
  return useQuery({
    queryKey: ["teams"],
    queryFn: async () =>
      (await api.get<Team[]>("/api/teams")).map((t) => ({
        ...t,
        budget_usd: t.budget_usd == null ? null : num(t.budget_usd),
        rpm_limit: t.rpm_limit == null ? null : num(t.rpm_limit),
        tpm_limit: t.tpm_limit == null ? null : num(t.tpm_limit),
      })),
  });
}
export const useCreateTeam = () =>
  useInvalidatingMutation(
    (b: Partial<Team>) => api.post("/api/teams", b),
    [["teams"]],
  );
export const useUpdateTeam = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Partial<Team>) =>
      api.patch(`/api/teams/${id}`, b),
    [["teams"]],
  );
export const useDeleteTeam = () =>
  useInvalidatingMutation(
    ({ id, revokeKeys }: { id: string; revokeKeys?: boolean }) =>
      api.del(`/api/teams/${id}${revokeKeys ? "?revoke_keys=true" : ""}`),
    [["teams"], ["keys"]],
  );

export function useTeamMembers(teamId: string | null) {
  return useQuery({
    queryKey: ["team-members", teamId],
    queryFn: async () =>
      (await api.get<TeamMember[]>(`/api/teams/${teamId}/members`)).map((m) => ({
        ...m,
        budget_usd: m.budget_usd == null ? null : num(m.budget_usd),
      })),
    enabled: !!teamId,
  });
}
export const useAddMember = (teamId: string) =>
  useInvalidatingMutation(
    (b: { user_id: string; role: string }) =>
      api.post(`/api/teams/${teamId}/members`, b),
    [["team-members", teamId]],
  );
export const useUpdateMember = (teamId: string) =>
  useInvalidatingMutation(
    ({ uid, ...b }: { uid: string; role?: string; budget_usd?: number | null; budget_period?: string | null }) =>
      api.patch(`/api/teams/${teamId}/members/${uid}`, b),
    [["team-members", teamId]],
  );
export const useRemoveMember = (teamId: string) =>
  useInvalidatingMutation(
    (uid: string) => api.del(`/api/teams/${teamId}/members/${uid}`),
    [["team-members", teamId]],
  );

// ── API keys ────────────────────────────────────────────────────
export function useKeys() {
  return useQuery({
    queryKey: ["keys"],
    queryFn: async () =>
      (await api.get<ApiKey[]>("/api/keys")).map((k) => ({
        ...k,
        budget_usd: k.budget_usd == null ? null : num(k.budget_usd),
        rpm_limit: k.rpm_limit == null ? null : num(k.rpm_limit),
        tpm_limit: k.tpm_limit == null ? null : num(k.tpm_limit),
      })),
  });
}
export const useCreateKey = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) =>
      api.post<{ id: string; key: string }>("/api/keys", b),
    [["keys"]],
  );
export const useUpdateKey = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Record<string, unknown>) =>
      api.patch(`/api/keys/${id}`, b),
    [["keys"]],
  );
export const useDeleteKey = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/keys/${id}`),
    [["keys"]],
  );

// ── Providers ───────────────────────────────────────────────────
export function useProviders(enabled = true) {
  return useQuery({
    queryKey: ["providers"],
    enabled,
    queryFn: () => api.get<Provider[]>("/api/providers"),
  });
}
export const useCreateProvider = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) => api.post("/api/providers", b),
    [["providers"]],
  );
export const useUpdateProvider = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Record<string, unknown>) =>
      api.patch(`/api/providers/${id}`, b),
    [["providers"]],
  );
export const useDeleteProvider = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/providers/${id}`),
    [["providers"], ["deployments"]],
  );

/** Live model/deployment discovery from the upstream provider. */
export interface DiscoveredModel {
  id: string;
  model?: string;
  input_per_1m?: number;
  output_per_1m?: number;
  cached_input_per_1m?: number;
  context_tokens?: number;
}

export function useProviderModels(providerId: string) {
  return useQuery({
    queryKey: ["provider-models", providerId],
    queryFn: () =>
      api.get<DiscoveredModel[]>(`/api/providers/${providerId}/models`),
    enabled: providerId !== "",
    retry: false,
    staleTime: 30_000,
  });
}

// ── Deployments ─────────────────────────────────────────────────
export interface CoolingDeployment {
  deployment_id: string;
  until: string;
  failures: number;
}

/** Deployments the gateway is holding out of routing after failures. */
export function useAvailableModels(enabled = true) {
  return useQuery({
    queryKey: ["available-models"],
    enabled,
    queryFn: async () =>
      (await api.get<AvailableModel[]>("/api/models/available")).map((m) => ({
        ...m,
        input_per_1m: m.input_per_1m == null ? null : num(m.input_per_1m),
        output_per_1m: m.output_per_1m == null ? null : num(m.output_per_1m),
      })),
    refetchInterval: 15_000,
  });
}
export function useRoutingHealth() {
  return useQuery({
    queryKey: ["routing-health"],
    queryFn: () => api.get<CoolingDeployment[]>("/api/routing/health"),
    refetchInterval: 5_000,
  });
}

export function useDeployments(enabled = true) {
  return useQuery({
    queryKey: ["deployments"],
    enabled,
    queryFn: async () =>
      (await api.get<Deployment[]>("/api/deployments")).map((d) => ({
        ...d,
        priority: num(d.priority),
        input_per_1m: d.input_per_1m == null ? null : num(d.input_per_1m),
        output_per_1m: d.output_per_1m == null ? null : num(d.output_per_1m),
        cached_input_per_1m:
          d.cached_input_per_1m == null ? null : num(d.cached_input_per_1m),
      })),
  });
}
export const useCreateDeployment = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) => api.post("/api/deployments", b),
    [["deployments"]],
  );
export const useUpdateDeployment = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Record<string, unknown>) =>
      api.patch(`/api/deployments/${id}`, b),
    [["deployments"]],
  );
export const useDeleteDeployment = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/deployments/${id}`),
    [["deployments"]],
  );

// ── Workspaces ──────────────────────────────────────────────────
export function useWorkspaces(enabled = true) {
  return useQuery({
    queryKey: ["workspaces"],
    queryFn: () => api.get<Workspace[]>("/api/workspaces"),
    enabled,
    // The container can stop underneath us, so status is polled rather than
    // trusted from the last mutation.
    refetchInterval: 10_000,
  });
}
export const useCreateWorkspace = () =>
  useInvalidatingMutation(
    (b: { name: string; image?: string; template_id?: string }) =>
      api.post("/api/workspaces", b),
    [["workspaces"]],
  );

export function useWorkspaceTemplates(enabled = true) {
  return useQuery({
    queryKey: ["workspace-templates"],
    queryFn: () => api.get<WorkspaceTemplate[]>("/api/workspace-templates"),
    enabled,
  });
}
type TemplateBody = {
  name: string;
  description: string;
  image: string;
  dockerfile: string;
};
export const useCreateWorkspaceTemplate = () =>
  useInvalidatingMutation(
    (b: TemplateBody) => api.post("/api/workspace-templates", b),
    [["workspace-templates"]],
  );
export const useUpdateWorkspaceTemplate = () =>
  useInvalidatingMutation(
    ({ id, ...b }: TemplateBody & { id: string }) =>
      api.patch(`/api/workspace-templates/${id}`, b),
    [["workspace-templates"]],
  );
export const useDeleteWorkspaceTemplate = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/workspace-templates/${id}`),
    [["workspace-templates"]],
  );

export function useWorkspaceUserFiles(enabled = true) {
  return useQuery({
    queryKey: ["workspace-user-files"],
    queryFn: () => api.get<WorkspaceUserFile[]>("/api/workspaces/files"),
    enabled,
  });
}
export const useUploadWorkspaceUserFiles = () =>
  useInvalidatingMutation(
    (form: FormData) => upload("PUT", "/api/workspaces/files", form),
    [["workspace-user-files"]],
  );
export const useDeleteWorkspaceUserFile = () =>
  useInvalidatingMutation(
    (path: string) =>
      api.del(`/api/workspaces/files?path=${encodeURIComponent(path)}`),
    [["workspace-user-files"]],
  );
export const useDeleteWorkspace = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/workspaces/${id}`),
    [["workspaces"]],
  );
export const useStartWorkspace = () =>
  useInvalidatingMutation(
    (id: string) => api.post(`/api/workspaces/${id}/start`, {}),
    [["workspaces"]],
  );
export const useStopWorkspace = () =>
  useInvalidatingMutation(
    (id: string) => api.post(`/api/workspaces/${id}/stop`, {}),
    [["workspaces"]],
  );

export function useWorkspaceFiles(id: string, path: string, enabled = true) {
  return useQuery({
    queryKey: ["workspace-files", id, path],
    queryFn: () =>
      api.get<WorkspaceEntry[]>(
        `/api/workspaces/${id}/files?path=${encodeURIComponent(path)}`,
      ),
    enabled,
  });
}

export function useWorkspaceSettings() {
  return useQuery({
    queryKey: ["workspace-settings"],
    queryFn: () => api.get<WorkspaceSettings>("/api/settings/workspaces"),
  });
}
export const useSaveWorkspaceSettings = () =>
  useInvalidatingMutation(
    (b: WorkspaceSettings) => api.put("/api/settings/workspaces", b),
    [["workspace-settings"], ["workspaces"]],
  );

// ── Pricing ─────────────────────────────────────────────────────
export function usePrices(enabled = true) {
  return useQuery({
    queryKey: ["prices"],
    enabled,
    queryFn: async () =>
      (await api.get<Price[]>("/api/prices")).map((p) => ({
        ...p,
        input_per_1m: num(p.input_per_1m),
        output_per_1m: num(p.output_per_1m),
        // NULL means "no cached rate", which must not become $0 (free).
        cached_input_per_1m:
          p.cached_input_per_1m == null ? null : num(p.cached_input_per_1m),
      })),
  });
}
export const useSetPrice = () =>
  useInvalidatingMutation(
    ({ model_id, ...b }: { model_id: string } & Record<string, unknown>) =>
      api.put(`/api/prices/${encodeURIComponent(model_id)}`, b),
    [["prices"]],
  );

// ── SSO settings ────────────────────────────────────────────────
export function useSso() {
  return useQuery({
    queryKey: ["sso"],
    queryFn: () => api.get<SsoSettings>("/api/settings/sso"),
  });
}
export const useSetSso = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) => api.put("/api/settings/sso", b),
    [["sso"]],
  );

// ── Telemetry settings ──────────────────────────────────────────
export function useTelemetry(enabled = true) {
  return useQuery({
    queryKey: ["telemetry"],
    enabled,
    queryFn: async () => {
      const d = await api.get<TelemetrySettings>("/api/settings/telemetry");
      // Legacy telemetry blobs predate the rules engine — default rules/mode so
      // every consumer (Observability page, Keys trace badges) can iterate safely.
      if (d.telemetry) {
        d.telemetry.rules = d.telemetry.rules ?? [];
        d.telemetry.mode = d.telemetry.mode || "everything";
      }
      return d;
    },
  });
}
export const useSetTelemetry = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) => api.put("/api/settings/telemetry", b),
    [["telemetry"]],
  );

// ── Token issuers (cloud token auth) ────────────────────────────
export function useTokenIssuers() {
  return useQuery({
    queryKey: ["token-issuers"],
    queryFn: () => api.get<TokenIssuer[]>("/api/token-issuers"),
  });
}
export const useCreateTokenIssuer = () =>
  useInvalidatingMutation(
    (b: Record<string, unknown>) => api.post("/api/token-issuers", b),
    [["token-issuers"]],
  );
export const useUpdateTokenIssuer = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Record<string, unknown>) =>
      api.patch(`/api/token-issuers/${id}`, b),
    [["token-issuers"]],
  );
export const useDeleteTokenIssuer = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/token-issuers/${id}`),
    [["token-issuers"]],
  );

// ── Edge nodes ──────────────────────────────────────────────────
export function useEdgeNodes() {
  return useQuery({
    queryKey: ["edge-nodes"],
    queryFn: () => api.get<EdgeNode[]>("/api/edge-nodes"),
    refetchInterval: 30_000,
  });
}
export const useCreateEdgeNode = () =>
  useInvalidatingMutation(
    (b: { name: string }) =>
      api.post<{ id: string; token: string }>("/api/edge-nodes", b),
    [["edge-nodes"]],
  );
export const useDeleteEdgeNode = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/edge-nodes/${id}`),
    [["edge-nodes"]],
  );

// ── Usage ───────────────────────────────────────────────────────
export function useUsageSummary(days: number) {
  return useQuery({
    queryKey: ["usage-summary", days],
    queryFn: async () => {
      const s = await api.get<UsageSummary>(`/api/usage/summary?days=${days}`);
      return {
        requests: num(s.requests),
        tokens: num(s.tokens),
        prompt_tokens: num(s.prompt_tokens),
        cached_tokens: num(s.cached_tokens),
        cost_usd: num(s.cost_usd),
        unpriced_requests: num(s.unpriced_requests),
        errors: num(s.errors),
      } satisfies UsageSummary;
    },
  });
}
export function useUsageDaily(days: number) {
  return useQuery({
    queryKey: ["usage-daily", days],
    queryFn: async () =>
      (await api.get<UsageDaily[]>(`/api/usage/daily?days=${days}`)).map(
        (d) => ({
          day: d.day,
          requests: num(d.requests),
          tokens: num(d.tokens),
          cost_usd: num(d.cost_usd),
        }),
      ),
  });
}
export function useUsageByModel(days: number) {
  return useQuery({
    queryKey: ["usage-by-model", days],
    queryFn: async () =>
      (await api.get<UsageByModel[]>(`/api/usage/by-model?days=${days}`)).map(
        (m) => ({
          model_name: m.model_name,
          requests: num(m.requests),
          prompt_tokens: num(m.prompt_tokens),
          completion_tokens: num(m.completion_tokens),
          cost_usd: num(m.cost_usd),
        }),
      ),
  });
}
export function useUsageRecent(limit: number) {
  return useQuery({
    queryKey: ["usage-recent", limit],
    queryFn: async () =>
      (await api.get<UsageRecent[]>(`/api/usage/recent?limit=${limit}`)).map(
        (r) => ({
          ...r,
          prompt_tokens: num(r.prompt_tokens),
          completion_tokens: num(r.completion_tokens),
          cost_usd: num(r.cost_usd),
          latency_ms: num(r.latency_ms),
          status_code: num(r.status_code),
        }),
      ),
    refetchInterval: 15_000,
  });
}

// ── Routing flow for the selected range (GET /api/usage/flow) ────
export function useUsageFlow(days: number) {
  return useQuery({
    queryKey: ["usage-flow", days],
    queryFn: async () => {
      const rows = await api.get<FlowEdgeRow[]>(`/api/usage/flow?days=${days}`);
      return (rows ?? []).map((r) => ({ ...r, volume: num(r.volume) }));
    },
  });
}

// ── Live metrics (admin) — Upstream-health, Nodes rps ────────────
export function useLiveMetrics(enabled = true) {
  return useQuery({
    queryKey: ["live-metrics"],
    enabled,
    queryFn: async () => {
      const m = await api.get<LiveMetrics>("/api/metrics/live");
      return {
        providers: (m.providers ?? []).map((p) => ({
          ...p,
          p50_ms: num(p.p50_ms),
          p95_ms: num(p.p95_ms),
          err_pct: num(p.err_pct),
          rpm: num(p.rpm),
          share_pct: num(p.share_pct),
        })),
        edges: (m.edges ?? []).map((e) => ({ ...e, volume: num(e.volume) })),
        nodes: (m.nodes ?? []).map((n) => ({ ...n, rps: num(n.rps) })),
        totals: {
          rpm: num(m.totals?.rpm),
          keys: num(m.totals?.keys),
          aliases: num(m.totals?.aliases),
          upstreams: num(m.totals?.upstreams),
        },
      } satisfies LiveMetrics;
    },
    refetchInterval: 5_000,
  });
}

// ── Spend by team (window=today|Nd) ──────────────────────────────
export function useSpendByTeam(window = "today") {
  return useQuery({
    queryKey: ["spend-by-team", window],
    queryFn: async () =>
      (
        await api.get<SpendByTeam[]>(
          `/api/usage/by-team?window=${encodeURIComponent(window)}`,
        )
      ).map((t) => ({ ...t, usd: num(t.usd), tokens: num(t.tokens) })),
  });
}

// ── Daily spend split by provider → pivoted for StackedProviderChart ──
// Backend returns long-form [{day, provider, cost_usd}]; we pivot to one row
// per day with a column per provider, and cap to the top 4 providers by spend
// (rest folded into "other") so the fixed 4-color palette stays legible.
export function useUsageByProviderDaily(days: number) {
  return useQuery({
    queryKey: ["usage-by-provider-daily", days],
    queryFn: async () => {
      const rows = await api.get<
        { day: string; provider: string; cost_usd: string | number }[]
      >(`/api/usage/by-provider-daily?days=${days}`);
      const totals = new Map<string, number>();
      for (const r of rows)
        totals.set(r.provider, (totals.get(r.provider) ?? 0) + num(r.cost_usd));
      const top = [...totals.entries()]
        .sort((a, b) => b[1] - a[1])
        .slice(0, 4)
        .map(([p]) => p);
      const keep = new Set(top);
      const byDay = new Map<string, Record<string, number>>();
      for (const r of rows) {
        const day = byDay.get(r.day) ?? {};
        const col = keep.has(r.provider) ? r.provider : "other";
        day[col] = (day[col] ?? 0) + num(r.cost_usd);
        byDay.set(r.day, day);
      }
      const providers = [...top];
      if (totals.size > top.length) providers.push("other");
      const data = [...byDay.entries()]
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([day, cols]) => ({ day, ...cols }));
      return { data, providers };
    },
  });
}

// ── Enriched requests feed + cursor pagination ───────────────────
function requestsQuery(f: RequestFilters): string {
  const p = new URLSearchParams();
  p.set("limit", String(f.limit ?? 50));
  if (f.key_id) p.set("key_id", f.key_id);
  if (f.user_id) p.set("user_id", f.user_id);
  if (f.model) p.set("model", f.model);
  if (f.status) p.set("status", f.status);
  if (f.tag) p.set("tag", f.tag);
  if (f.since) p.set("since", f.since);
  if (f.until) p.set("until", f.until);
  // Forwarded verbatim so an analytics drill-down narrows to the same rows the
  // aggregate counted; the server ignores anything it does not recognise.
  for (const k of [
    "team_id",
    "provider_id",
    "deployment_id",
    "error_code",
    "priced",
    "stream",
    "failover",
    "days",
    "request_id",
  ] as const) {
    const v = f[k];
    if (v) p.set(k, String(v));
  }
  return p.toString();
}
function coerceRequest(r: RequestRow): RequestRow {
  return {
    ...r,
    id: num(r.id),
    prompt_tokens: num(r.prompt_tokens),
    completion_tokens: num(r.completion_tokens),
    cost_usd: num(r.cost_usd),
    latency_ms: num(r.latency_ms),
    status_code: num(r.status_code),
    // null stays null: rows written before migration 0008 have no measurement,
    // which is not the same as a measured zero.
    attempts: r.attempts == null ? null : num(r.attempts),
    ttft_ms: r.ttft_ms == null ? null : num(r.ttft_ms),
    notional_cost_usd:
      r.notional_cost_usd == null ? null : num(r.notional_cost_usd),
  };
}
/**
 * Infinite feed of enriched request rows. Pages flatten via
 * `data.pages.flat()`. Next cursor is built from the last row as
 * `before=${ts},${id}`; a short page ends pagination.
 */
export function useRequests(filters: RequestFilters = {}) {
  const limit = filters.limit ?? 50;
  return useInfiniteQuery({
    queryKey: ["requests", filters],
    initialPageParam: "" as string,
    queryFn: async ({ pageParam }) => {
      const qs = requestsQuery(filters);
      const before = pageParam
        ? `&before=${encodeURIComponent(pageParam)}`
        : "";
      const rows = await api.get<RequestRow[]>(
        `/api/usage/requests?${qs}${before}`,
      );
      return rows.map(coerceRequest);
    },
    getNextPageParam: (last) => {
      if (last.length < limit) return undefined;
      const row = last[last.length - 1];
      return `${row.ts},${row.id}`;
    },
    refetchInterval: 15_000,
  });
}

// ── Deployment req counts (admin) ────────────────────────────────
export function useUsageByDeployment(days = 1) {
  return useQuery({
    queryKey: ["usage-by-deployment", days],
    queryFn: async () =>
      (
        await api.get<DeploymentReq[]>(`/api/usage/by-deployment?days=${days}`)
      ).map((d) => ({
        deployment_id: d.deployment_id,
        requests: num(d.requests),
      })),
  });
}

// ── Nodes ops (admin) — self + edges + cluster events ────────────
export function useNodes() {
  return useQuery({
    queryKey: ["nodes"],
    queryFn: async () => {
      const r = await api.get<NodesResponse>("/api/nodes");
      return {
        nodes: (r.nodes ?? []).map((n) => ({
          ...n,
          uptime_s: num(n.uptime_s),
          cpu_pct: num(n.cpu_pct),
          mem_pct: num(n.mem_pct),
          rps: num(n.rps),
          p50_ms: num(n.p50_ms),
        })) as Node[],
        cluster_events: r.cluster_events ?? [],
      } satisfies NodesResponse;
    },
    refetchInterval: 10_000,
  });
}
// cluster_events ride the /api/nodes response — no separate hook. Read
// `useNodes().data?.cluster_events`.

// ── Observability: exported-now ring (admin) ─────────────────────
export function useObservabilityExported() {
  return useQuery({
    queryKey: ["obs-exported"],
    queryFn: async () =>
      (await api.get<ExportedEvent[]>("/api/observability/exported")).map(
        (e) => ({
          ...e,
          latency_ms: num(e.latency_ms),
          cost_usd: num(e.cost_usd),
        }),
      ),
    refetchInterval: 5_000,
  });
}

// ── Observability: per-rule matching-rpm + optional preview ──────
export function useObservabilityRuleStats(preview?: {
  scope_type: string;
  scope_value: string;
}) {
  return useQuery({
    queryKey: ["obs-rule-stats", preview ?? null],
    queryFn: async () => {
      const q =
        preview && preview.scope_type && preview.scope_value
          ? `?scope_type=${encodeURIComponent(preview.scope_type)}&scope_value=${encodeURIComponent(
              preview.scope_value,
            )}`
          : "";
      const r = await api.get<RuleStats>(`/api/observability/rules/stats${q}`);
      return {
        rules: (r.rules ?? []).map((s) => ({
          ...s,
          matching_rpm: num(s.matching_rpm),
        })),
        total_rpm: num(r.total_rpm),
        preview: r.preview
          ? { ...r.preview, matching_rpm: num(r.preview.matching_rpm) }
          : undefined,
      } satisfies RuleStats;
    },
    refetchInterval: 5_000,
  });
}

// ── Observability: test connection (always HTTP 200; check `ok`) ──
export function useTestObservability() {
  return useMutation({
    mutationFn: (b: Record<string, unknown>) =>
      api.post<ObservabilityTest>("/api/observability/test", b),
  });
}

// ── Chat playground ─────────────────────────────────────────────
export function useChatModels() {
  return useQuery({
    queryKey: ["chat-models"],
    queryFn: () => api.get<string[]>("/api/chat/models"),
  });
}
/**
 * Saved conversations. Chat history is owned by a user row, so bootstrap-admin
 * token sessions (which have no user) get a 403 — pass `enabled: false` for
 * those rather than firing a request that can only fail.
 */
export function useChats(enabled = true) {
  return useQuery({
    queryKey: ["chats"],
    queryFn: () => api.get<ChatSummary[]>("/api/chats"),
    enabled,
  });
}
export function useChat(id: string, enabled = true) {
  return useQuery({
    queryKey: ["chat", id],
    queryFn: () => api.get<ChatDetail>(`/api/chats/${id}`),
    enabled: enabled && !!id,
  });
}
export const useCreateChat = () =>
  useInvalidatingMutation(
    (b: { title?: string; model?: string }) =>
      api.post<{ id: string }>("/api/chats", b),
    [["chats"]],
  );
export const useUpdateChat = () =>
  useInvalidatingMutation(
    ({ id, ...b }: { id: string } & Record<string, unknown>) =>
      api.put(`/api/chats/${id}`, b),
    [["chats"]],
  );
export const useDeleteChat = () =>
  useInvalidatingMutation(
    (id: string) => api.del(`/api/chats/${id}`),
    [["chats"]],
  );

// ── Analytics ────────────────────────────────────────────────────
/** Serialises the filter state; blank values are omitted so URLs stay readable. */
export function usageQueryString(q: UsageQuery): string {
  const p = new URLSearchParams({ days: String(q.days) });
  for (const [k, v] of Object.entries(q)) {
    if (k !== "days" && v) p.set(k, String(v));
  }
  return p.toString();
}

export function useUsageStats(q: UsageQuery) {
  const qs = usageQueryString(q);
  return useQuery({
    queryKey: ["usage-stats", qs],
    queryFn: async () => {
      const s = await api.get<UsageStats>(`/api/usage/stats?${qs}`);
      return Object.fromEntries(
        Object.entries(s).map(([k, v]) => [k, num(v as number)]),
      ) as unknown as UsageStats;
    },
  });
}

export function useUsageBreakdown(q: UsageQuery, groupBy: BreakdownDim) {
  const qs = `${usageQueryString(q)}&group_by=${groupBy}`;
  return useQuery({
    queryKey: ["usage-breakdown", qs],
    queryFn: async () => {
      const rows = await api.get<BreakdownRow[]>(`/api/usage/breakdown?${qs}`);
      return (rows ?? []).map((r) => ({
        ...r,
        requests: num(r.requests),
        prompt_tokens: num(r.prompt_tokens),
        completion_tokens: num(r.completion_tokens),
        cached_tokens: num(r.cached_tokens),
        cost_usd: num(r.cost_usd),
        notional_cost_usd: num(r.notional_cost_usd),
        errors: num(r.errors),
        failovers: num(r.failovers),
        p50_ms: num(r.p50_ms),
        p95_ms: num(r.p95_ms),
        ttft_p50_ms: num(r.ttft_p50_ms),
      }));
    },
  });
}

export function useUsageTimeseries(q: UsageQuery) {
  const qs = usageQueryString(q);
  return useQuery({
    queryKey: ["usage-timeseries", qs],
    queryFn: async () => {
      const rows = await api.get<TimeseriesPoint[]>(
        `/api/usage/timeseries?${qs}`,
      );
      return (rows ?? []).map((r) => ({
        ...r,
        requests: num(r.requests),
        tokens: num(r.tokens),
        cost_usd: num(r.cost_usd),
        errors: num(r.errors),
      }));
    },
  });
}
