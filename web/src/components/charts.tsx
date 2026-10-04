import type { ReactNode } from "react";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Icon } from "@astryxdesign/core/Icon";
import { Divider } from "@astryxdesign/core/Divider";

/**
 * Recharts is not token-aware, so every colour is threaded through as a CSS
 * custom property reference. This keeps charts in step with light/dark mode
 * and with any future theme swap, without a JS colour lookup.
 */
export const chart = {
  accent: "var(--color-accent)",
  grid: "var(--color-border)",
  axis: "var(--color-text-secondary)",
  good: "var(--color-success)",
  crit: "var(--color-error)",
  info: "var(--color-text-blue)",
  neutral: "var(--color-text-disabled)",
};

const axisTick = { fontSize: 11, fill: chart.axis };

/**
 * Chart panel. The sized box lives here, but ResponsiveContainer deliberately
 * does NOT: it clones its direct child to inject width/height, so it must wrap
 * the recharts element itself, never an intermediate component. Each chart
 * below owns its own ResponsiveContainer.
 */
export function ChartCard({
  title,
  action,
  children,
  height = 240,
}: {
  title: string;
  action?: ReactNode;
  children: ReactNode;
  height?: number;
}) {
  return (
    <Card>
      <VStack gap={4}>
        <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
          <Heading level={4}>{title}</Heading>
          {action}
        </HStack>
        {/* min-width:0 lets the chart shrink inside a flex/grid parent instead
            of forcing the card wider than the viewport on small screens. */}
        <div style={{ width: "100%", minWidth: 0, height }}>{children}</div>
      </VStack>
    </Card>
  );
}

/** Shared tooltip surface so charts match popovers elsewhere in the console. */
export function ChartTooltip({
  active,
  payload,
  label,
  format,
}: {
  active?: boolean;
  payload?: Array<{ name?: string; value?: number | string; color?: string }>;
  label?: string | number;
  format?: (v: number | string, name?: string) => string;
}) {
  if (!active || !payload?.length) return null;
  return (
    <Card padding={2}>
      <VStack gap={1}>
        <Text type="supporting" color="secondary">
          {String(label ?? "")}
        </Text>
        {payload.map((p, i) => (
          <HStack key={i} gap={2} vAlign="center">
            <span
              aria-hidden
              style={{
                width: 8,
                height: 8,
                borderRadius: "var(--radius-full)",
                background: p.color,
                display: "inline-block",
              }}
            />
            <Text type="supporting">
              {format && p.value != null ? format(p.value, p.name) : `${p.name}: ${p.value}`}
            </Text>
          </HStack>
        ))}
      </VStack>
    </Card>
  );
}

export function SpendArea({
  data,
  xKey,
  yKey,
  formatValue,
}: {
  data: Array<Record<string, unknown>>;
  xKey: string;
  yKey: string;
  formatValue: (v: number | string) => string;
}) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <AreaChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
      <defs>
        <linearGradient id="spendFill" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={chart.accent} stopOpacity={0.28} />
          <stop offset="100%" stopColor={chart.accent} stopOpacity={0} />
        </linearGradient>
      </defs>
      <CartesianGrid horizontal vertical={false} stroke={chart.grid} />
      <XAxis dataKey={xKey} tick={axisTick} axisLine={false} tickLine={false} minTickGap={24} />
      <YAxis tick={axisTick} axisLine={false} tickLine={false} width={64} tickFormatter={formatValue} />
      <Tooltip content={<ChartTooltip format={(v) => formatValue(v)} />} cursor={{ stroke: chart.grid }} />
      <Area
        type="monotone"
        dataKey={yKey}
        stroke={chart.accent}
        strokeWidth={2}
        fill="url(#spendFill)"
        isAnimationActive={false}
      />
      </AreaChart>
    </ResponsiveContainer>
  );
}

export function TokensBars({
  data,
  xKey,
  series,
}: {
  data: Array<Record<string, unknown>>;
  xKey: string;
  series: Array<{ key: string; name: string; color: string }>;
}) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <BarChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
      <CartesianGrid horizontal vertical={false} stroke={chart.grid} />
      <XAxis dataKey={xKey} tick={axisTick} axisLine={false} tickLine={false} minTickGap={24} />
      <YAxis tick={axisTick} axisLine={false} tickLine={false} width={52} />
      <Tooltip content={<ChartTooltip />} cursor={{ fill: "var(--color-overlay-hover)" }} />
      {series.map((s, i) => (
        <Bar
          key={s.key}
          dataKey={s.key}
          name={s.name}
          stackId="t"
          fill={s.color}
          isAnimationActive={false}
          radius={i === series.length - 1 ? [3, 3, 0, 0] : undefined}
        />
      ))}
      </BarChart>
    </ResponsiveContainer>
  );
}

export function LatencyLine({
  data,
  xKey,
  yKey,
}: {
  data: Array<Record<string, unknown>>;
  xKey: string;
  yKey: string;
}) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid horizontal vertical={false} stroke={chart.grid} />
        <XAxis dataKey={xKey} tick={axisTick} axisLine={false} tickLine={false} minTickGap={24} />
        <YAxis tick={axisTick} axisLine={false} tickLine={false} width={52} />
        <Tooltip content={<ChartTooltip />} cursor={{ stroke: chart.grid }} />
        <Line type="monotone" dataKey={yKey} stroke={chart.info} strokeWidth={2} dot={false} isAnimationActive={false} />
      </LineChart>
    </ResponsiveContainer>
  );
}

/** Horizontal bar breakdown (spend by model / by team). */
export function Breakdown({
  data,
  labelKey,
  valueKey,
  format,
}: {
  data: Array<Record<string, unknown>>;
  labelKey: string;
  valueKey: string;
  format: (v: number) => string;
}) {
  const max = Math.max(1, ...data.map((d) => Number(d[valueKey]) || 0));
  return (
    <VStack gap={2}>
      {data.map((d, i) => {
        const v = Number(d[valueKey]) || 0;
        return (
          <VStack key={i} gap={1}>
            <HStack hAlign="between" gap={3}>
              <Text type="supporting">{String(d[labelKey])}</Text>
              <span className="tnum">
                <Text type="supporting" color="secondary">
                  {format(v)}
                </Text>
              </span>
            </HStack>
            <div
              aria-hidden
              style={{
                height: 6,
                width: "100%",
                background: "var(--color-track)",
                borderRadius: "var(--radius-full)",
                overflow: "hidden",
              }}
            >
              <div
                style={{
                  height: "100%",
                  width: `${(v / max) * 100}%`,
                  background: chart.accent,
                  borderRadius: "var(--radius-full)",
                }}
              />
            </div>
          </VStack>
        );
      })}
    </VStack>
  );
}

export function Sparkline({ data, color = chart.accent }: { data: number[]; color?: string }) {
  const rows = data.map((v, i) => ({ i, v }));
  return (
    <div style={{ width: "100%", height: 36 }}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={rows} margin={{ top: 2, right: 0, left: 0, bottom: 2 }}>
          <Line type="monotone" dataKey="v" stroke={color} strokeWidth={1.5} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}

/** KPI tile: big number, monospace, optional sparkline and delta. */
export function Stat({
  label,
  value,
  hint,
  tone,
  spark,
  icon,
}: {
  label: string;
  value: string;
  hint?: string;
  tone?: "good" | "crit" | "accent";
  spark?: number[];
  icon?: React.ComponentType<React.SVGProps<SVGSVGElement>>;
}) {
  const color =
    tone === "good" ? chart.good : tone === "crit" ? chart.crit : tone === "accent" ? chart.accent : undefined;
  return (
    <Card>
      <VStack gap={2}>
        <HStack gap={2} vAlign="center" hAlign="between">
          <span className="eyebrow">{label}</span>
          {icon && <Icon icon={icon} size="sm" color="secondary" />}
        </HStack>
        <span className="tnum" style={{ color, fontSize: "var(--font-size-2xl)", fontWeight: 600 }}>
          {value}
        </span>
        {hint && (
          <Text type="supporting" color="secondary">
            {hint}
          </Text>
        )}
        {spark && spark.length > 1 && (
          <>
            <Divider />
            <Sparkline data={spark} color={color ?? chart.accent} />
          </>
        )}
      </VStack>
    </Card>
  );
}

export { Cell };
