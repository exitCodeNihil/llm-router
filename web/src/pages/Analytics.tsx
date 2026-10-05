import { useMemo, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Divider } from "@astryxdesign/core/Divider";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@astryxdesign/core/SegmentedControl";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import {
  Table,
  pixel,
  proportional,
  type TableColumn,
} from "@astryxdesign/core/Table";
import { Link } from "react-router-dom";

import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { ChartCard, SpendArea, Stat } from "../components/charts";
import {
  useAvailableModels,
  useMe,
  useDeployments,
  useKeys,
  useProviders,
  useTeams,
  useUsageBreakdown,
  useUsageStats,
  useUsageTimeseries,
  usageQueryString,
} from "../lib/hooks";
import type { BreakdownDim, BreakdownRow, UsageQuery } from "../lib/types";
import { plural, compact, dateShort, money, int } from "../lib/format";

const RANGES = [
  { v: "1", label: "24h" },
  { v: "7", label: "7d" },
  { v: "30", label: "30d" },
  { v: "90", label: "90d" },
];

const DIMS: { v: BreakdownDim; label: string }[] = [
  { v: "model", label: "Model" },
  { v: "provider", label: "Provider" },
  { v: "deployment", label: "Backend" },
  { v: "key", label: "API key" },
  { v: "team", label: "Team" },
  { v: "user", label: "User" },
  { v: "error_code", label: "Error reason" },
  { v: "status", label: "Status" },
  { v: "edge_node", label: "Edge node" },
];

const METRICS = [
  { v: "cost_usd", label: "Spend" },
  { v: "requests", label: "Requests" },
  { v: "tokens", label: "Tokens" },
] as const;

export default function Analytics() {
  const [q, setQ] = useState<UsageQuery>({ days: 30 });
  const [dim, setDim] = useState<BreakdownDim>("model");
  const [metric, setMetric] =
    useState<(typeof METRICS)[number]["v"]>("cost_usd");

  const stats = useUsageStats(q);
  const rows = useUsageBreakdown(q, dim);
  const series = useUsageTimeseries(q);

  const isAdmin = !!useMe().data?.is_admin;
  const providers = useProviders(isAdmin);
  const deployments = useDeployments(isAdmin);
  const available = useAvailableModels(!isAdmin);
  const keys = useKeys();
  const teams = useTeams();

  const set = <K extends keyof UsageQuery>(k: K, v: UsageQuery[K]) =>
    setQ((cur) => ({ ...cur, [k]: v }));

  // Which filters are actually narrowing the result, for the "clear" affordance
  // and so it is obvious a number is not the whole picture.
  const active = useMemo(
    () =>
      Object.entries(q)
        .filter(([k, v]) => k !== "days" && v)
        .map(([k, v]) => `${k}=${v}`),
    [q],
  );

  const modelOptions = useMemo(
    () => [
      { value: "", label: "All models" },
      ...[
        ...new Set([
          ...(deployments.data ?? []).map((d) => d.model_name),
          ...(available.data ?? []).map((m) => m.name),
        ]),
      ]
        // Usage rows carry the requested name, never a pattern like "claude-*".
        .filter((m) => !m.endsWith("*"))
        .sort()
        .map((m) => ({ value: m, label: m })),
    ],
    [deployments.data, available.data],
  );

  // Axis labels: raw bucket timestamps render as 2026-07-24T04:00:00+04:00.
  // Hourly buckets need the time, daily ones do not.
  const hourly = q.days <= 2;
  const points = useMemo(
    () =>
      (series.data ?? []).map((p) => ({
        ...p,
        label: hourly
          ? new Date(p.bucket).toLocaleTimeString([], {
              hour: "2-digit",
              minute: "2-digit",
            })
          : dateShort(p.bucket),
      })),
    [series.data, hourly],
  );

  const s = stats.data;
  const promptTokens = s?.prompt_tokens ?? 0;
  const cacheRate = promptTokens
    ? Math.round(((s?.cached_tokens ?? 0) / promptTokens) * 100)
    : 0;

  // Drill-down keeps the filters: the row list must describe the same set as the
  // number above it, or the two views quietly disagree.
  const requestsHref = `/requests?${usageQueryString(q)}`;

  const columns: TableColumn<BreakdownRow & Record<string, unknown>>[] = [
    {
      key: "label",
      header: DIMS.find((d) => d.v === dim)?.label ?? "Bucket",
      width: proportional(2),
      renderCell: (r) => (
        <Text type="body" maxLines={1}>
          {r.label}
        </Text>
      ),
    },
    {
      key: "requests",
      header: "Requests",
      width: pixel(96),
      align: "end",
      renderCell: (r) => <span className="tnum">{int(r.requests)}</span>,
    },
    {
      key: "tokens",
      header: "In / Out",
      width: pixel(112),
      align: "end",
      // Split, because the two move independently: a caching client sends huge
      // prompts and tiny completions, and one combined figure hides that.
      renderCell: (r) => (
        <span className="tnum">
          {compact(r.prompt_tokens)} / {compact(r.completion_tokens)}
        </span>
      ),
    },
    {
      key: "cached_tokens",
      header: "Cached",
      width: pixel(84),
      align: "end",
      renderCell: (r) => (
        <Text type="supporting" color="secondary">
          {r.prompt_tokens
            ? `${Math.round((r.cached_tokens / r.prompt_tokens) * 100)}%`
            : "—"}
        </Text>
      ),
    },
    {
      key: "cost_usd",
      header: "Spend",
      width: pixel(96),
      align: "end",
      renderCell: (r) => <span className="tnum">{money(r.cost_usd)}</span>,
    },
    {
      key: "notional_cost_usd",
      header: "List value",
      width: pixel(96),
      align: "end",
      renderCell: (r) => (
        <Text type="supporting" color="secondary">
          {r.notional_cost_usd ? `≈${money(r.notional_cost_usd)}` : "—"}
        </Text>
      ),
    },
    {
      key: "errors",
      header: "Errors",
      width: pixel(84),
      align: "end",
      renderCell: (r) =>
        r.errors ? (
          <Badge variant="error" label={int(r.errors)} />
        ) : (
          <span
            className="tnum"
            style={{ color: "var(--color-text-disabled)" }}
          >
            0
          </span>
        ),
    },
    {
      key: "p95_ms",
      header: "p95",
      width: pixel(84),
      align: "end",
      renderCell: (r) => <span className="tnum">{int(r.p95_ms)} ms</span>,
    },
    {
      key: "ttft_p50_ms",
      header: "TTFT",
      width: pixel(84),
      align: "end",
      renderCell: (r) => (
        <span className="tnum">
          {r.ttft_p50_ms ? `${int(r.ttft_p50_ms)} ms` : "—"}
        </span>
      ),
    },
  ];

  return (
    <Page
      eyebrow="Overview"
      title="Analytics"
      description="Slice traffic, spend and latency by any dimension, then drill into the matching requests."
    >
      <VStack gap={4}>
        {/* ── Filters ───────────────────────────────────────────── */}
        <Card>
          <VStack gap={3}>
            <HStack hAlign="between" vAlign="center" wrap="wrap" gap={2}>
              <SegmentedControl
                label="Time range"
                value={String(q.days)}
                onChange={(v) => set("days", Number(v))}
              >
                {RANGES.map((r) => (
                  <SegmentedControlItem key={r.v} value={r.v} label={r.label} />
                ))}
              </SegmentedControl>
              <HStack gap={2} vAlign="center">
                {active.length > 0 && (
                  <Text type="supporting" color="secondary">
                    {active.length} filter{active.length > 1 ? "s" : ""} active
                  </Text>
                )}
                <Button
                  label="Clear filters"
                  variant="ghost"
                  size="sm"
                  isDisabled={active.length === 0}
                  onClick={() => setQ({ days: q.days })}
                />
              </HStack>
            </HStack>

            <Grid columns={{ minWidth: 170, repeat: "fit" }} gap={3}>
              <Selector
                label="Model"
                value={q.model ?? ""}
                onChange={(v) => set("model", v)}
                hasSearch={modelOptions.length > 8}
                options={modelOptions}
              />
              <Selector
                label="Provider"
                value={q.provider_id ?? ""}
                onChange={(v) => set("provider_id", v)}
                options={[
                  { value: "", label: "All providers" },
                  ...(providers.data ?? []).map((p) => ({
                    value: p.id,
                    label: p.name,
                  })),
                ]}
              />
              <Selector
                label="API key"
                value={q.key_id ?? ""}
                onChange={(v) => set("key_id", v)}
                hasSearch={(keys.data ?? []).length > 8}
                options={[
                  { value: "", label: "All keys" },
                  ...(keys.data ?? []).map((k) => ({
                    value: k.id,
                    label: k.name || k.key_prefix,
                  })),
                ]}
              />
              <Selector
                label="Team"
                value={q.team_id ?? ""}
                onChange={(v) => set("team_id", v)}
                options={[
                  { value: "", label: "All teams" },
                  ...(teams.data ?? []).map((t) => ({
                    value: t.id,
                    label: t.name,
                  })),
                ]}
              />
              <Selector
                label="Outcome"
                value={q.status ?? ""}
                onChange={(v) => set("status", v as UsageQuery["status"])}
                options={[
                  { value: "", label: "Any outcome" },
                  { value: "ok", label: "Succeeded" },
                  { value: "err", label: "Failed" },
                ]}
              />
              <Selector
                label="Pricing"
                value={q.priced ?? ""}
                onChange={(v) => set("priced", v as UsageQuery["priced"])}
                options={[
                  { value: "", label: "Any pricing" },
                  { value: "priced", label: "Priced" },
                  { value: "unpriced", label: "Unpriced (subscription)" },
                ]}
              />
            </Grid>
          </VStack>
        </Card>

        {/* ── Headline ──────────────────────────────────────────── */}
        {stats.error ? (
          <ErrorState error={stats.error} onRetry={() => stats.refetch()} />
        ) : stats.isLoading || !s ? (
          <Loading label="Aggregating" />
        ) : (
          <Grid columns={{ minWidth: 168, repeat: "fit" }} gap={4}>
            <Stat
              label="Spend"
              value={money(s.cost_usd)}
              hint={
                s.notional_cost_usd
                  ? `+ ${money(s.notional_cost_usd)} list value, unbilled`
                  : s.unpriced_requests
                    ? `${int(s.unpriced_requests)} unpriced`
                    : "all priced"
              }
            />
            <Stat
              label="Requests"
              value={compact(s.requests)}
              hint={plural(s.models, "model")}
            />
            <Stat
              label="Tokens"
              value={`${compact(s.prompt_tokens)} / ${compact(s.completion_tokens)}`}
              hint={
                cacheRate
                  ? `in / out · ${cacheRate}% of prompt cached`
                  : "in / out"
              }
            />
            <Stat
              label="Errors"
              value={int(s.errors)}
              hint={
                s.requests
                  ? `${((s.errors / s.requests) * 100).toFixed(1)}% error rate`
                  : "—"
              }
            />
            <Stat
              label="Latency"
              value={`${int(s.p95_ms)} ms`}
              hint={`p95 · p50 ${int(s.p50_ms)} ms`}
            />
            <Stat
              label="TTFT"
              value={s.ttft_p50_ms ? `${int(s.ttft_p50_ms)} ms` : "—"}
              hint={
                s.failovers ? `${int(s.failovers)} failovers` : "no failover"
              }
            />
          </Grid>
        )}

        {/* ── Trend ─────────────────────────────────────────────── */}
        <ChartCard
          title="Trend"
          height={220}
          action={
            <SegmentedControl
              label="Metric"
              value={metric}
              onChange={(v) => setMetric(v as typeof metric)}
            >
              {METRICS.map((m) => (
                <SegmentedControlItem key={m.v} value={m.v} label={m.label} />
              ))}
            </SegmentedControl>
          }
        >
          {series.error ? (
            <ErrorState error={series.error} onRetry={() => series.refetch()} />
          ) : series.isLoading ? (
            <Loading label="Loading" />
          ) : (series.data?.length ?? 0) === 0 ? (
            <Empty
              title="No traffic in this window"
              description="Widen the range or clear a filter."
            />
          ) : (
            <SpendArea
              data={points as unknown as Array<Record<string, unknown>>}
              xKey="label"
              yKey={
                metric === "tokens"
                  ? "tokens"
                  : metric === "requests"
                    ? "requests"
                    : "cost_usd"
              }
              formatValue={(v) =>
                metric === "cost_usd" ? money(Number(v)) : compact(Number(v))
              }
            />
          )}
        </ChartCard>

        {/* ── Breakdown ─────────────────────────────────────────── */}
        <Card padding={0}>
          <VStack gap={0}>
            <HStack
              padding={4}
              hAlign="between"
              vAlign="center"
              wrap="wrap"
              gap={2}
            >
              <HStack gap={3} vAlign="center">
                <Heading level={4}>Breakdown</Heading>
                <Selector
                  label="Group by"
                  value={dim}
                  onChange={(v) => setDim(v as BreakdownDim)}
                  options={DIMS.map((d) => ({ value: d.v, label: d.label }))}
                />
              </HStack>
              <Link to={requestsHref} className="link">
                <Text type="supporting">View matching requests →</Text>
              </Link>
            </HStack>
            <Divider />
            {rows.error ? (
              <ErrorState error={rows.error} onRetry={() => rows.refetch()} />
            ) : rows.isLoading ? (
              <Loading />
            ) : (rows.data?.length ?? 0) === 0 ? (
              <Empty
                title="Nothing matches these filters"
                description="Clear a filter or widen the time range."
              />
            ) : (
              <Table
                data={
                  (rows.data ?? []) as Array<
                    BreakdownRow & Record<string, unknown>
                  >
                }
                columns={columns}
                idKey="bucket"
                density="compact"
                dividers="rows"
              />
            )}
          </VStack>
        </Card>
      </VStack>
    </Page>
  );
}
