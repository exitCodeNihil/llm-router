import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Card } from "@astryxdesign/core/Card";
import { Grid } from "@astryxdesign/core/Grid";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { StatusDot } from "@astryxdesign/core/StatusDot";
import { Badge } from "@astryxdesign/core/Badge";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { Divider } from "@astryxdesign/core/Divider";

import { useMe,
  useLiveMetrics,
  useSpendByTeam,
  useUsageByModel,
  useUsageByProviderDaily,
  useUsageDaily,
  useUsageRecent,
  useUsageFlow,
  useUsageSummary,
} from "../lib/hooks";
import type { FlowEdgeRow, UsageByModel, UsageRecent } from "../lib/types";
import { compact, dateShort, int, money, plural } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Breakdown, ChartCard, SpendArea, Stat, TokensBars, chart } from "../components/charts";
import { Sankey, type SankeyEdge, type SankeyNode } from "../components/Sankey";

type Metric = "cost_usd" | "requests" | "tokens";

// Categorical hues for provider series, from the Astryx data palette.
const PROVIDER_COLORS = [
  "var(--color-icon-blue)",
  "var(--color-accent)",
  "var(--color-icon-green)",
  "var(--color-icon-purple)",
  "var(--color-text-disabled)",
];

const trunc = (s: string, n: number) => (s.length > n ? `${s.slice(0, n - 1)}…` : s);

/**
 * Collapse the live edge list into three columns of nodes plus aggregated
 * links. Only the five busiest keys get their own row; the rest fold into a
 * "+N keys" node so the diagram stays readable under load.
 */
function buildFlow(rows: FlowEdgeRow[], liveLed: Map<string, SankeyNode["led"]>) {
  // History outlives keys, so deleted ones fold into a single node rather than
  // one identical-looking row per departed key id.
  const DELETED = "deleted";
  const keyIdOf = (e: FlowEdgeRow) => (e.key_name === "(deleted key)" ? DELETED : e.api_key_id);

  const byKey = new Map<string, { name: string; vol: number }>();
  for (const e of rows) {
    const id = keyIdOf(e);
    const k = byKey.get(id) ?? { name: e.key_name, vol: 0 };
    k.vol += e.volume;
    byKey.set(id, k);
  }

  const ranked = [...byKey.entries()].sort((a, b) => b[1].vol - a[1].vol);
  const top = ranked.slice(0, 5);
  const extra = ranked.length - top.length;
  const topIds = new Set(top.map(([id]) => id));

  const keys: SankeyNode[] = top.map(([id, k]) => ({
    id: `k:${id}`,
    label: id === DELETED ? "deleted keys" : trunc(k.name, 12),
  }));
  if (extra > 0) keys.push({ id: "k:+more", label: `+${extra} keys` });

  const aliases: SankeyNode[] = [...new Set(rows.map((e) => e.model_name))].map((mm) => ({
    id: `m:${mm}`,
    label: trunc(mm, 11),
  }));

  // Providers that actually appear in the range, ordered by volume so colours
  // stay stable as the range changes. led means "serving traffic right now",
  // which is still live information even when the volumes are historical.
  const provVol = new Map<string, { name: string; vol: number }>();
  for (const e of rows) {
    const p = provVol.get(e.provider_id) ?? { name: e.provider_name, vol: 0 };
    p.vol += e.volume;
    provVol.set(e.provider_id, p);
  }
  const provRanked = [...provVol.entries()].sort((a, b) => b[1].vol - a[1].vol);
  const upstreams: SankeyNode[] = provRanked.map(([id, p]) => ({
    id: `u:${id}`,
    label: trunc(p.name, 10),
    led: liveLed.get(id),
  }));

  const colorOf = new Map(
    provRanked.map(([id], i) => [id, PROVIDER_COLORS[i % PROVIDER_COLORS.length]]),
  );

  const agg = new Map<string, SankeyEdge>();
  const add = (from: string, to: string, volume: number, color?: string) => {
    const cur = agg.get(`${from}|${to}`);
    if (cur) cur.volume += volume;
    else agg.set(`${from}|${to}`, { from, to, volume, color });
  };
  for (const e of rows) {
    const id = keyIdOf(e);
    add(topIds.has(id) ? `k:${id}` : "k:+more", `m:${e.model_name}`, e.volume);
    add(`m:${e.model_name}`, `u:${e.provider_id}`, e.volume, colorOf.get(e.provider_id));
  }

  return { keys, aliases, upstreams, edges: [...agg.values()] };
}

// ── Routing flow ─────────────────────────────────────────
function RoutingFlow({ days, rangeLabel, isAdmin }: { days: number; rangeLabel: string; isAdmin: boolean }) {
  const flowRows = useUsageFlow(days);
  const live = useLiveMetrics(isAdmin);
  // Volumes are historical; the LED still reports how each upstream is doing
  // right now, which is the one genuinely live thing on this card.
  const liveLed = useMemo(
    () => new Map((live.data?.providers ?? []).map((p) => [p.id, p.led])),
    [live.data],
  );
  const flow = useMemo(
    () => (flowRows.data ? buildFlow(flowRows.data, liveLed) : null),
    [flowRows.data, liveLed],
  );
  const hasTraffic = !!flow && flow.edges.length > 0;

  return (
    <Card>
      <VStack gap={3}>
        <HStack hAlign="between" vAlign="center">
          <Heading level={4}>Routing flow</Heading>
          {flow && (
            <Text type="supporting" color="secondary">
              {plural(flow.keys.length, "key")} · {plural(flow.upstreams.length, "upstream")} · {rangeLabel}
            </Text>
          )}
        </HStack>
        {flowRows.isLoading ? (
          <Loading label="Loading" />
        ) : !hasTraffic ? (
          <Text type="supporting" color="secondary">
            No traffic in {rangeLabel.toLowerCase()} — the flow appears as requests arrive.
          </Text>
        ) : (
          <Sankey {...flow} />
        )}
      </VStack>
    </Card>
  );
}

// ── Daily spend by provider ──────────────────────────────────
function ProviderSpend({ days }: { days: number }) {
  const byProvider = useUsageByProviderDaily(days);

  const rows = useMemo(
    () => (byProvider.data?.data ?? []).map((d) => ({ ...d, label: dateShort(d.day) })),
    [byProvider.data],
  );

  const series = (byProvider.data?.providers ?? []).map((p, i) => ({
    key: p,
    name: p,
    color: PROVIDER_COLORS[i % PROVIDER_COLORS.length],
  }));

  if (byProvider.isLoading) return <Loading />;
  if (byProvider.error)
    return <ErrorState error={byProvider.error} onRetry={() => byProvider.refetch()} />;

  return (
    <ChartCard title="Daily spend by provider" height={220}>
      {rows.length === 0 ? (
        <Empty title="No provider spend in this range" />
      ) : (
        <TokensBars data={rows as Array<Record<string, unknown>>} xKey="label" series={series} />
      )}
    </ChartCard>
  );
}

const RANGES = [
  { v: "1", label: "24h" },
  { v: "7", label: "7d" },
  { v: "30", label: "30d" },
  { v: "90", label: "90d" },
];

// ── Upstream health ─────────────────────────────────────────────
function UpstreamHealth() {
  const live = useLiveMetrics();
  const providers = live.data?.providers ?? [];

  return (
    <Card>
      <VStack gap={3}>
        <HStack hAlign="between" vAlign="center">
          <Heading level={4}>Upstream health</Heading>
          {live.data && (
            <Text type="supporting" color="secondary">
              {int(live.data.totals.rpm)} rpm
            </Text>
          )}
        </HStack>
        {live.isLoading ? (
          <Loading label="Sampling" />
        ) : providers.length === 0 ? (
          <Text type="supporting" color="secondary">
            No upstream traffic in the last minute.
          </Text>
        ) : (
          <VStack gap={2}>
            {providers.map((p) => (
              <HStack key={p.id} gap={3} vAlign="center" hAlign="between">
                <HStack gap={2} vAlign="center">
                  <StatusDot
                    variant={p.led === "ok" ? "success" : p.led === "warn" ? "warning" : "error"}
                    label={`${p.name} ${p.led}`}
                    isPulsing={p.led !== "ok"}
                  />
                  <Text type="supporting" maxLines={1}>
                    {p.name}
                  </Text>
                </HStack>
                <span className="tnum">
                  <Text type="supporting" color="secondary">
                    p95 {int(p.p95_ms)}ms · {p.err_pct.toFixed(1)}% err
                  </Text>
                </span>
              </HStack>
            ))}
          </VStack>
        )}
      </VStack>
    </Card>
  );
}

// ── Spend by team ───────────────────────────────────────────────
function SpendByTeam() {
  const spend = useSpendByTeam("today");
  const rows = (spend.data ?? []).slice(0, 6);
  return (
    <Card>
      <VStack gap={3}>
        <Heading level={4}>Spend by team · today</Heading>
        {spend.isLoading ? (
          <Loading label="Loading" />
        ) : rows.length === 0 ? (
          <Text type="supporting" color="secondary">
            No team-attributed spend today.
          </Text>
        ) : (
          <Breakdown
            data={rows as unknown as Array<Record<string, unknown>>}
            labelKey="team_name"
            valueKey="usd"
            format={(v) => money(v)}
          />
        )}
      </VStack>
    </Card>
  );
}

// ── Recent requests ─────────────────────────────────────────────
const recentColumns: TableColumn<UsageRecent & Record<string, unknown>>[] = [
  {
    key: "ts",
    header: "Time",
    width: pixel(112),
    renderCell: (r) => <Timestamp value={r.ts} format="relative" isLive />,
  },
  { key: "model_name", header: "Model", width: proportional(1) },
  {
    key: "status_code",
    header: "Status",
    width: pixel(92),
    renderCell: (r) =>
      r.status_code >= 400 ? (
        <Badge variant="error" label={String(r.status_code)} />
      ) : (
        <Badge variant="success" label={String(r.status_code)} />
      ),
  },
  {
    key: "latency_ms",
    header: "Latency",
    width: pixel(96),
    align: "end",
    renderCell: (r) => `${int(r.latency_ms)} ms`,
  },
  {
    key: "tokens",
    header: "Tokens",
    width: pixel(96),
    align: "end",
    renderCell: (r) => compact(r.prompt_tokens + r.completion_tokens),
  },
  {
    key: "cost_usd",
    header: "Cost",
    width: pixel(100),
    align: "end",
    renderCell: (r) => money(r.cost_usd, { precise: true }),
  },
];

function RecentRequests() {
  const recent = useUsageRecent(12);
  return (
    <Card padding={0}>
      <VStack gap={0}>
        <HStack hAlign="between" vAlign="center" padding={4}>
          <Heading level={4}>Recent requests</Heading>
          <Link to="/requests">
            <Text type="supporting" color="accent">
              View all →
            </Text>
          </Link>
        </HStack>
        <Divider />
        {recent.isLoading ? (
          <Loading />
        ) : recent.error ? (
          <ErrorState error={recent.error} onRetry={() => recent.refetch()} />
        ) : (recent.data?.length ?? 0) === 0 ? (
          <Empty
            title="No requests yet"
            description="Requests appear here in near-real-time as the gateway serves them."
          />
        ) : (
          <Table
            data={(recent.data ?? []) as Array<UsageRecent & Record<string, unknown>>}
            columns={recentColumns}
            idKey={(r) => `${r.ts}-${r.model_name}-${r.latency_ms}`}
            density="compact"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </VStack>
    </Card>
  );
}

// ── Spend by model ──────────────────────────────────────────────
const modelColumns: TableColumn<UsageByModel & Record<string, unknown>>[] = [
  { key: "model_name", header: "Model", width: proportional(1) },
  {
    key: "requests",
    header: "Reqs",
    width: pixel(80),
    align: "end",
    renderCell: (m) => int(m.requests),
  },
  {
    key: "cost_usd",
    header: "Cost",
    width: pixel(96),
    align: "end",
    renderCell: (m) => money(m.cost_usd),
  },
];

// ── Page ────────────────────────────────────────────────────────
export default function Dashboard() {
  const isAdmin = !!useMe().data?.is_admin;
  const [days, setDays] = useState(30);
  // Same label the range control shows, so the card cannot claim a different window.
  const rangeLabel = RANGES.find((r) => r.v === String(days))?.label ?? `${days}d`;
  const [metric, setMetric] = useState<Metric>("cost_usd");

  const summary = useUsageSummary(days);
  const daily = useUsageDaily(days);
  const byModel = useUsageByModel(days);

  const trend = useMemo(
    () =>
      (daily.data ?? []).map((d) => ({
        ...d,
        label: dateShort(d.day),
      })),
    [daily.data],
  );

  const spark = (key: Metric) => (daily.data ?? []).map((d) => d[key]);

  const errorRate =
    summary.data && summary.data.requests
      ? (summary.data.errors / summary.data.requests) * 100
      : 0;

  return (
    <Page
      eyebrow="Overview"
      title="Dashboard"
      description="Traffic, spend, and latency across everything the gateway routed."
      action={
        <SegmentedControl
          label="Time range"
          size="sm"
          value={String(days)}
          onChange={(v) => setDays(Number(v))}
        >
          {RANGES.map((r) => (
            <SegmentedControlItem key={r.v} value={r.v} label={r.label} />
          ))}
        </SegmentedControl>
      }
    >
      {summary.isLoading ? (
        <Loading />
      ) : summary.error || !summary.data ? (
        <ErrorState error={summary.error} onRetry={() => summary.refetch()} />
      ) : (
        <VStack gap={4}>
          {/* KPI tiles */}
          <Grid columns={{ minWidth: 210, repeat: "fit" }} gap={4}>
            <Stat
              label="Spend"
              value={money(summary.data.cost_usd)}
              hint={
                summary.data.unpriced_requests
                  ? `${int(summary.data.unpriced_requests)} unpriced`
                  : "all requests priced"
              }
              tone="accent"
              spark={spark("cost_usd")}
            />
            <Stat
              label="Requests"
              value={int(summary.data.requests)}
              hint={`over ${days} day${days === 1 ? "" : "s"}`}
              spark={spark("requests")}
            />
            <Stat
              label="Tokens"
              value={compact(summary.data.tokens)}
              hint={
                summary.data.cached_tokens
                  ? `${Math.round((summary.data.cached_tokens / summary.data.prompt_tokens) * 100)}% of prompt tokens cached`
                  : "prompt + completion"
              }
              spark={spark("tokens")}
            />
            <Stat
              label="Errors"
              value={int(summary.data.errors)}
              tone={summary.data.errors > 0 ? "crit" : "good"}
              hint={summary.data.requests ? `${errorRate.toFixed(1)}% error rate` : "no traffic"}
            />
          </Grid>

          {/* Live cells */}
          <Grid columns={{ minWidth: 300, repeat: "fit" }} gap={4}>
            <RoutingFlow days={days} rangeLabel={rangeLabel} isAdmin={isAdmin} />
            {isAdmin && <UpstreamHealth />}
            <SpendByTeam />
          </Grid>

          {/* Trend + by-model */}
          <Grid columns={{ minWidth: 320, repeat: "fit" }} gap={4}>
            <ChartCard
              title="Daily trend"
              height={260}
              action={
                <SegmentedControl label="Metric" size="sm" value={metric} onChange={(v) => setMetric(v as Metric)}>
                  <SegmentedControlItem value="cost_usd" label="Spend" />
                  <SegmentedControlItem value="requests" label="Requests" />
                  <SegmentedControlItem value="tokens" label="Tokens" />
                </SegmentedControl>
              }
            >
              {metric === "cost_usd" ? (
                <SpendArea data={trend} xKey="label" yKey="cost_usd" formatValue={(v) => money(Number(v))} />
              ) : metric === "requests" ? (
                <TokensBars
                  data={trend}
                  xKey="label"
                  series={[{ key: "requests", name: "Requests", color: chart.accent }]}
                />
              ) : (
                <TokensBars
                  data={trend}
                  xKey="label"
                  series={[{ key: "tokens", name: "Tokens", color: chart.info }]}
                />
              )}
            </ChartCard>

            <Card padding={0}>
              <VStack gap={0}>
                <HStack padding={4}>
                  <Heading level={4}>Spend by model</Heading>
                </HStack>
                <Divider />
                {byModel.isLoading ? (
                  <Loading />
                ) : (byModel.data?.length ?? 0) === 0 ? (
                  <Empty title="No usage in this range" description="Route traffic through the gateway to see it here." />
                ) : (
                  <Table
                    data={(byModel.data ?? []) as Array<UsageByModel & Record<string, unknown>>}
                    columns={modelColumns}
                    idKey="model_name"
                    density="compact"
                    dividers="rows"
                    hasHover
                    textOverflow="truncate"
                  />
                )}
              </VStack>
            </Card>
          </Grid>

          <ProviderSpend days={days} />

          <RecentRequests />
        </VStack>
      )}
    </Page>
  );
}
