import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Card } from "@astryxdesign/core/Card";
import { Grid } from "@astryxdesign/core/Grid";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { Switch } from "@astryxdesign/core/Switch";
import { Divider } from "@astryxdesign/core/Divider";
import { Banner } from "@astryxdesign/core/Banner";
import { Slider } from "@astryxdesign/core/Slider";
import { StatusDot } from "@astryxdesign/core/StatusDot";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { useToast } from "@astryxdesign/core/Toast";

import type {
  Capture,
  DestinationScope,
  DestinationStatus,
  ExportedEvent,
  RuleScope,
  TelemetryDestination,
  TelemetryKind,
  TelemetryMode,
  TelemetryRule,
} from "../lib/types";
import {
  useDeleteDestination,
  useDestinations,
  useGatewayGroups,
  useKeys,
  useObservabilityExported,
  useObservabilityRuleStats,
  useSetTelemetry,
  useTeams,
  useTelemetry,
  useTelemetryKinds,
  useTestDestination,
  useUpdateDestination,
  useUsers,
} from "../lib/hooks";
import { int, money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm } from "../components/dialogs";
import { CONTROL_PLANE, DestinationDialog } from "../components/DestinationDialog";
import { IconEdit, IconPlus, IconTrash } from "../components/icons";

const SCOPES: { value: RuleScope; label: string }[] = [
  { value: "team", label: "Team" },
  { value: "user", label: "User" },
  { value: "key", label: "API key" },
  { value: "tag", label: "Tag" },
];

// ── Destinations ────────────────────────────────────────────────
const STATUS: Record<DestinationStatus, { variant: "success" | "error" | "neutral"; label: string }> = {
  healthy: { variant: "success", label: "Healthy" },
  failing: { variant: "error", label: "Failing" },
  no_data: { variant: "neutral", label: "No data" },
  disabled: { variant: "neutral", label: "Disabled" },
};

function hostOf(d: TelemetryDestination) {
  const u = d.endpoint || d.traces_url;
  try {
    return new URL(u).host;
  } catch {
    return u;
  }
}

/** "All teams", "Platform, Research", "All groups except Control plane". */
function scopeSummary(s: DestinationScope, noun: string, name: (v: string) => string) {
  const names = s.values.map(name);
  const list = names.length <= 2 ? names.join(", ") : `${names.length} ${noun}s`;
  if (s.mode === "all") return { short: `All ${noun}s`, full: "" };
  if (s.mode === "selected")
    return names.length === 0
      ? { short: `No ${noun}s`, full: "" }
      : { short: list, full: names.join(", ") };
  return names.length === 0
    ? { short: `All ${noun}s`, full: "" }
    : { short: `All ${noun}s except ${list}`, full: names.join(", ") };
}

/**
 * A tooltip over plain text. The Tooltip opens on focus only for a focusable
 * trigger, so the cell takes a tab stop and a name of its own; the tooltip
 * then describes it.
 */
function CellTip({ content, label, children }: { content: ReactNode; label: string; children: ReactNode }) {
  return (
    <Tooltip content={content}>
      <span tabIndex={0} role="group" aria-label={label} style={{ display: "flex", minWidth: 0, maxWidth: "100%" }}>
        {children}
      </span>
    </Tooltip>
  );
}

function StatusCell({ d }: { d: TelemetryDestination }) {
  // The row's own flag wins: right after a toggle the exporter may still
  // report the old worker's health until the config reloads. A destination
  // that can't run (undecryptable secrets) is failing whatever its counters
  // say.
  const status: DestinationStatus = !d.enabled ? "disabled" : d.error ? "failing" : d.health.status;
  const s = STATUS[status] ?? STATUS.no_data;
  const h = d.health;
  const dropped = h.dropped.overflow + h.dropped.rejected + h.dropped.expired;
  const lines = [
    d.error,
    h.last_error && `Last error${h.last_error_at ? ` at ${new Date(h.last_error_at).toLocaleString()}` : ""}: ${h.last_error}`,
    h.last_success_at && `Last delivered at ${new Date(h.last_success_at).toLocaleString()}`,
    `${int(h.delivered)} delivered, ${int(dropped)} dropped${h.queued ? `, ${int(h.queued)} queued` : ""} since start`,
  ].filter(Boolean) as string[];
  return (
    <CellTip
      label={`${d.name} status: ${s.label}`}
      content={<VStack gap={1}>{lines.map((l) => <span key={l}>{l}</span>)}</VStack>}
    >
      <HStack gap={1.5} vAlign="center">
        <StatusDot variant={s.variant} label={s.label} />
        <Text type="supporting" color={status === "failing" ? undefined : "secondary"}>
          {s.label}
        </Text>
      </HStack>
    </CellTip>
  );
}

function DestinationsCard({ policyCapturesContent }: { policyCapturesContent: boolean }) {
  const destinations = useDestinations();
  const kinds = useTelemetryKinds();
  const teams = useTeams();
  const groups = useGatewayGroups();
  const update = useUpdateDestination();
  const del = useDeleteDestination();
  const test = useTestDestination();
  const showToast = useToast();

  // undefined = closed, null = adding, a destination = editing.
  const [editing, setEditing] = useState<TelemetryDestination | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<TelemetryDestination | null>(null);
  const [toggling, setToggling] = useState<TelemetryDestination | null>(null);
  const [testing, setTesting] = useState<string | null>(null);

  const kindOf = (name: string): TelemetryKind | undefined => kinds.data?.find((k) => k.name === name);
  const teamName = (id: string) => teams.data?.find((t) => t.id === id)?.name ?? id;
  const groupName = (id: string) =>
    id === CONTROL_PLANE ? "Control plane" : (groups.data?.find((g) => g.id === id)?.name ?? id);

  async function toggle(d: TelemetryDestination) {
    try {
      // PUT is a full replacement. Secrets and headers left out keep their
      // stored values.
      await update.mutateAsync({
        id: d.id,
        name: d.name,
        kind: d.kind,
        enabled: !d.enabled,
        endpoint: d.endpoint,
        traces_url: d.traces_url,
        config: d.config,
        content_mode: d.content_mode,
        max_content_bytes: d.max_content_bytes,
        team_scope: d.team_scope,
        group_scope: d.group_scope,
      });
      showToast({ body: d.enabled ? `${d.name} disabled` : `${d.name} enabled` });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  async function runTest(d: TelemetryDestination) {
    setTesting(d.id);
    try {
      const r = await test.mutateAsync({ id: d.id });
      showToast({
        body: r.ok ? `${d.name}: test span accepted` : `${d.name}: ${r.status ? `HTTP ${r.status}, ` : ""}${r.detail}`,
        type: r.ok ? "info" : "error",
        endContent: r.trace_url ? (
          <a href={r.trace_url} target="_blank" rel="noreferrer" className="link">
            <Text type="supporting">Open trace</Text>
          </a>
        ) : undefined,
      });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    } finally {
      setTesting(null);
    }
  }

  const columns: TableColumn<TelemetryDestination & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Name",
      width: proportional(1.2),
      renderCell: (d) => (
        <Text type="body" weight="medium" maxLines={1}>
          {d.name}
        </Text>
      ),
    },
    {
      key: "kind",
      header: "Kind",
      width: pixel(150),
      renderCell: (d) => {
        const k = kindOf(d.kind);
        return (
          <CellTip content={k?.label ?? d.kind} label={`Kind: ${d.kind}${k?.beta ? ", beta" : ""}`}>
            <HStack gap={1} vAlign="center">
              <Badge variant="neutral" label={d.kind} />
              {k?.beta && <Badge variant="info" label="Beta" />}
            </HStack>
          </CellTip>
        );
      },
    },
    {
      key: "endpoint",
      header: "Endpoint",
      width: proportional(1.2),
      renderCell: (d) => <Code>{hostOf(d)}</Code>,
    },
    {
      key: "content_mode",
      header: "Content",
      width: pixel(104),
      renderCell: (d) =>
        d.content_mode === "content" ? (
          <Badge variant="purple" label="full" />
        ) : (
          <Badge variant="neutral" label="metadata" />
        ),
    },
    {
      key: "scope",
      header: "Scope",
      width: proportional(1.4),
      renderCell: (d) => {
        const t = scopeSummary(d.team_scope, "team", teamName);
        const g = scopeSummary(d.group_scope, "group", groupName);
        const full = [t.full && `Teams: ${t.short}`, g.full && `Groups: ${g.short}`].filter(Boolean).join(" · ");
        const cell = (
          <VStack gap={0}>
            <Text type="supporting" maxLines={1}>
              {t.short}
            </Text>
            <Text type="supporting" color="secondary" maxLines={1}>
              {g.short}
            </Text>
          </VStack>
        );
        return full ? (
          <CellTip content={full} label={`Scope: ${t.short}; ${g.short}`}>
            {cell}
          </CellTip>
        ) : (
          cell
        );
      },
    },
    {
      key: "status",
      header: "Status",
      width: pixel(112),
      renderCell: (d) => <StatusCell d={d} />,
    },
    {
      key: "enabled",
      header: "Enabled",
      width: pixel(84),
      renderCell: (d) => (
        <Switch
          label={`${d.enabled ? "Disable" : "Enable"} ${d.name}`}
          isLabelHidden
          value={d.enabled}
          changeAction={() => setToggling(d)}
        />
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(144),
      align: "end",
      renderCell: (d) => (
        <HStack gap={1} hAlign="end" vAlign="center">
          <Button
            label={`Test ${d.name}`}
            variant="ghost"
            size="sm"
            isLoading={testing === d.id}
            onClick={() => runTest(d)}
          >
            Test
          </Button>
          <IconButton
            label={`Edit ${d.name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconEdit} size="sm" />}
            onClick={() => setEditing(d)}
          />
          <IconButton
            label={`Delete ${d.name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={() => setDeleting(d)}
          />
        </HStack>
      ),
    },
  ];

  const addButton = (
    <Button
      label="Add destination"
      variant="secondary"
      icon={<Icon icon={IconPlus} size="sm" />}
      isDisabled={!kinds.data?.length}
      onClick={() => setEditing(null)}
    />
  );

  return (
    <Card padding={0}>
      <VStack gap={0}>
        <HStack padding={4} hAlign="between" vAlign="center" gap={3} wrap="wrap">
          <VStack gap={0}>
            <Heading level={4}>Destinations</Heading>
            <Text type="supporting" color="secondary">
              Each destination receives the traffic the capture policy exports, narrowed by its own scope.
            </Text>
          </VStack>
          {(destinations.data?.length ?? 0) > 0 && addButton}
        </HStack>
        <Divider />
        {destinations.isLoading ? (
          <Loading />
        ) : destinations.error ? (
          <ErrorState error={destinations.error} onRetry={() => destinations.refetch()} />
        ) : (destinations.data?.length ?? 0) === 0 ? (
          <Empty
            title="No destinations"
            description="Add an OpenTelemetry collector or a Langfuse project to start exporting traces."
            action={addButton}
          />
        ) : (
          <Table
            data={(destinations.data ?? []) as Array<TelemetryDestination & Record<string, unknown>>}
            columns={columns}
            idKey="id"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </VStack>

      {editing !== undefined && kinds.data && (
        <DestinationDialog
          editing={editing}
          kinds={kinds.data}
          policyCapturesContent={policyCapturesContent}
          onClose={() => setEditing(undefined)}
        />
      )}

      <Confirm
        target={toggling}
        title={toggling?.enabled ? "Disable destination" : "Enable destination"}
        description={
          toggling?.enabled
            ? `Stop exporting to "${toggling?.name}"? Traces already sent stay there.`
            : `Start exporting to "${toggling?.name}"? Matching traffic${
                toggling?.content_mode === "content" ? ", including prompt content where the policy captures it," : ""
              } is sent to ${toggling ? hostOf(toggling) : ""}.`
        }
        actionLabel={toggling?.enabled ? "Disable" : "Enable"}
        isLoading={update.isPending}
        onCancel={() => setToggling(null)}
        onConfirm={async () => {
          if (!toggling) return;
          await toggle(toggling);
          setToggling(null);
        }}
      />

      <Confirm
        target={deleting}
        title="Delete destination"
        description={`Delete "${deleting?.name}"? Export to it stops and its stored secrets are removed. Traces already sent stay there.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Destination deleted" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setDeleting(null);
        }}
      />
    </Card>
  );
}

export default function Observability() {
  const tel = useTelemetry();
  const save = useSetTelemetry();
  const destinations = useDestinations();
  const teams = useTeams();
  const users = useUsers();
  const keys = useKeys();
  const exported = useObservabilityExported();
  const stats = useObservabilityRuleStats();
  const showToast = useToast();

  const [captureContent, setCaptureContent] = useState(false);
  const [mode, setMode] = useState<TelemetryMode>("everything");
  const [rules, setRules] = useState<TelemetryRule[]>([]);

  // Hydrate once.
  const hydrated = useRef(false);
  useEffect(() => {
    const t = tel.data;
    if (!t || hydrated.current) return;
    hydrated.current = true;
    setCaptureContent(t.capture_content);
    setMode((t.mode || "everything") as TelemetryMode);
    setRules([...t.rules].sort((a, b) => a.order - b.order));
  }, [tel.data]);

  // ── Rule draft ───────────────────────────────────────────────
  const [scopeType, setScopeType] = useState<RuleScope>("team");
  const [scopeValue, setScopeValue] = useState("");
  const [draftCapture, setDraftCapture] = useState<Capture>("meta");
  const [draftSample, setDraftSample] = useState(100);
  const preview = useObservabilityRuleStats(
    scopeValue ? { scope_type: scopeType, scope_value: scopeValue } : undefined,
  );

  const patchRule = (id: string, patch: Partial<TelemetryRule>) =>
    setRules(rules.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  function move(id: string, dir: -1 | 1) {
    const i = rules.findIndex((r) => r.id === id);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= rules.length) return;
    const next = [...rules];
    [next[i], next[j]] = [next[j], next[i]];
    setRules(next.map((r, idx) => ({ ...r, order: idx })));
  }

  const removeRule = (id: string) =>
    setRules(rules.filter((r) => r.id !== id).map((r, idx) => ({ ...r, order: idx })));

  function addRule() {
    if (!scopeValue.trim()) {
      showToast({ body: "Pick what the rule applies to", type: "error" });
      return;
    }
    const rule: TelemetryRule = {
      id: crypto.randomUUID(),
      order: rules.length,
      scope_type: scopeType,
      scope_value: scopeValue.trim(),
      capture: draftCapture,
      sample: draftSample,
      enabled: true,
    };
    setRules([...rules, rule]);
    setScopeValue("");
  }

  // Options for the scope picker follow the selected scope type.
  const scopeOptions = useMemo(() => {
    if (scopeType === "team") return (teams.data ?? []).map((t) => ({ value: t.id, label: t.name }));
    if (scopeType === "user") return (users.data ?? []).map((u) => ({ value: u.id, label: u.email }));
    if (scopeType === "key")
      return (keys.data ?? []).map((k) => ({ value: k.id, label: k.name || k.key_prefix }));
    return [];
  }, [scopeType, teams.data, users.data, keys.data]);

  const scopeLabel = (r: TelemetryRule) => {
    if (r.scope_type === "team") return teams.data?.find((t) => t.id === r.scope_value)?.name ?? r.scope_value;
    if (r.scope_type === "user") return users.data?.find((u) => u.id === r.scope_value)?.email ?? r.scope_value;
    if (r.scope_type === "key")
      return keys.data?.find((k) => k.id === r.scope_value)?.name ?? r.scope_value;
    return r.scope_value;
  };

  const rpmFor = (id: string) => stats.data?.rules.find((r) => r.rule_id === id)?.matching_rpm ?? 0;

  // ── Pending policy changes ───────────────────────────────────
  // Capture policy decides whether prompts and completions leave the gateway,
  // so it is edited as a draft and applied explicitly: a mis-click here is a
  // data-egress change, not a display preference.
  const saved = tel.data;
  const changes = useMemo(() => {
    if (!saved) return [];
    const out: string[] = [];
    const savedMode = (saved.mode || "everything") as TelemetryMode;
    const label: Record<TelemetryMode, string> = {
      everything: "Everything",
      by_rule: "By rule",
      off: "Off",
    };
    if (savedMode !== mode) out.push(`Mode: ${label[savedMode]} → ${label[mode]}`);
    if (mode === "everything" && saved.capture_content !== captureContent)
      out.push(`Prompt content: ${saved.capture_content ? "captured" : "metadata only"} → ${captureContent ? "captured" : "metadata only"}`);

    const before = new Map((saved.rules ?? []).map((r) => [r.id, r]));
    const after = new Map(rules.map((r) => [r.id, r]));
    for (const r of rules) {
      const b = before.get(r.id);
      if (!b) {
        out.push(`Add rule: ${r.scope_type} ${scopeLabel(r)} → ${r.capture} at ${r.sample}%`);
        continue;
      }
      if (b.capture !== r.capture) out.push(`${scopeLabel(r)}: capture ${b.capture} → ${r.capture}`);
      if (b.sample !== r.sample) out.push(`${scopeLabel(r)}: sample ${b.sample}% → ${r.sample}%`);
      if (b.enabled !== r.enabled) out.push(`${scopeLabel(r)}: ${r.enabled ? "enabled" : "disabled"}`);
      if (b.order !== r.order) out.push(`${scopeLabel(r)}: reordered to #${r.order + 1}`);
    }
    for (const b of saved.rules ?? []) if (!after.has(b.id)) out.push(`Remove rule: ${b.scope_type} ${scopeLabel(b)}`);
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saved, mode, captureContent, rules, teams.data, users.data, keys.data]);

  // Requests per minute that would start or stop having content captured.
  const affected = useMemo(() => {
    if (mode === "off") return 0;
    if (mode === "everything") return stats.data?.total_rpm ?? 0;
    return rules.filter((r) => r.enabled).reduce((n, r) => n + rpmFor(r.id) * (r.sample / 100), 0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, rules, stats.data]);

  async function savePolicy() {
    try {
      await save.mutateAsync({ mode, capture_content: captureContent, rules });
      showToast({ body: "Capture policy applied" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  function discardPolicy() {
    if (!saved) return;
    setMode((saved.mode || "everything") as TelemetryMode);
    setCaptureContent(saved.capture_content);
    setRules([...(saved.rules ?? [])].sort((a, b) => a.order - b.order));
  }

  // Whether the stored policy can let content out at all, for the hint in the
  // destination dialog.
  const policyCapturesContent =
    !!saved &&
    (saved.mode === "everything"
      ? saved.capture_content
      : saved.mode === "by_rule" && saved.rules.some((r) => r.enabled && r.capture === "content"));
  const anyEnabled = (destinations.data ?? []).some((d) => d.enabled);
  const failing = (destinations.data ?? []).filter((d) => d.enabled && (d.health.status === "failing" || d.error));

  const eventColumns: TableColumn<ExportedEvent & Record<string, unknown>>[] = [
    {
      key: "ts",
      header: "Time",
      width: pixel(110),
      renderCell: (e) => <Timestamp value={e.ts} format="time" />,
    },
    { key: "key_name", header: "Key", width: proportional(1) },
    {
      key: "model",
      header: "Model",
      width: proportional(1.2),
      renderCell: (e) => <Code>{e.model}</Code>,
    },
    {
      key: "capture",
      header: "Capture",
      width: pixel(96),
      renderCell: (e) => (
        <Badge variant={e.capture === "content" ? "purple" : "neutral"} label={e.capture} />
      ),
    },
    {
      key: "latency_ms",
      header: "Latency",
      width: pixel(92),
      align: "end",
      renderCell: (e) => `${int(e.latency_ms)} ms`,
    },
    {
      key: "cost_usd",
      header: "Cost",
      width: pixel(96),
      align: "end",
      renderCell: (e) => money(e.cost_usd, { precise: true }),
    },
  ];

  if (tel.isLoading) return <Loading />;
  if (tel.error) return <ErrorState error={tel.error} onRetry={() => tel.refetch()} />;

  return (
    <Page
      eyebrow="Operations"
      title="Observability"
      description="Export request traces to OpenTelemetry backends and Langfuse. The capture policy decides what may leave the gateway; each destination then picks its traffic and whether content goes with it."
    >
      <VStack gap={4}>
        {/* Delivery health. The live feed below lists events *selected* for
            export, which looks the same whether a destination accepted them
            or rejected every one, so failures are stated outright. */}
        {failing.length > 0 && (
          <Banner
            status="error"
            title={
              failing.length === 1
                ? `${failing[0].name} is failing`
                : `${failing.length} destinations are failing`
            }
            description={`${failing
              .map((d) => `${d.name}: ${d.error || d.health.last_error || "export failed"}`)
              .join(" · ")} — use Test to check the endpoint and credentials.`}
          />
        )}
        {anyEnabled && mode === "by_rule" && rules.length === 0 && changes.length === 0 && (
          <Banner
            status="warning"
            title="Export is on, but no rules exist"
            description="In by-rule mode traffic matching nothing is not exported, so nothing reaches any destination. Add a rule for a team, user or key below, or switch to Everything."
          />
        )}

        <DestinationsCard policyCapturesContent={policyCapturesContent} />

        {/* Capture policy */}
        <Card>
          <VStack gap={4}>
            <VStack gap={0}>
              <Heading level={4}>Capture policy</Heading>
              <Text type="supporting" color="secondary">
                Applies before every destination: traffic it does not export reaches none of them, and content it does
                not capture is never sent.
              </Text>
            </VStack>

            <VStack gap={2}>
              <span className="eyebrow">Mode</span>
              <SegmentedControl
                label="Capture mode"
                layout="fill"
                value={mode}
                onChange={(v) => setMode(v as TelemetryMode)}
              >
                <SegmentedControlItem value="everything" label="Everything" />
                <SegmentedControlItem value="by_rule" label="By rule" />
                <SegmentedControlItem value="off" label="Off" />
              </SegmentedControl>
            </VStack>

            {mode === "everything" && (
              <Switch
                label="Capture prompt content"
                description="Off exports metadata only: no prompts or completions leave the gateway, whatever a destination asks for"
                value={captureContent}
                onChange={setCaptureContent}
              />
            )}

            {mode === "off" && (
              <Banner status="info" title="Export is off" description="No trace events are being produced." />
            )}

            {mode === "by_rule" && (
              <VStack gap={3}>
                <Text type="supporting" color="secondary">
                  Rules are evaluated in order; the first match wins. Traffic matching nothing is not exported.
                </Text>

                {rules.length === 0 ? (
                  <Text type="supporting" color="secondary">
                    No rules yet — nothing is being exported.
                  </Text>
                ) : (
                  <VStack gap={2}>
                    {rules.map((r, i) => (
                      <VStack key={r.id} gap={2}>
                        <HStack gap={2} vAlign="center" wrap="wrap">
                          <Badge variant="neutral" label={`${i + 1}`} />
                          <Badge variant="blue" label={r.scope_type} />
                          <Text type="supporting" maxLines={1}>
                            {scopeLabel(r)}
                          </Text>
                          <Badge
                            variant={r.capture === "content" ? "purple" : "neutral"}
                            label={r.capture}
                          />
                          <span className="tnum">
                            <Text type="supporting" color="secondary">
                              {r.sample}% · ~{int(rpmFor(r.id))} rpm
                            </Text>
                          </span>
                          <Switch
                            label={`${r.enabled ? "Disable" : "Enable"} rule ${i + 1}`}
                            isLabelHidden
                            value={r.enabled}
                            changeAction={() => patchRule(r.id, { enabled: !r.enabled })}
                          />
                          <Button
                            label="↑"
                            variant="ghost"
                            size="sm"
                            isDisabled={i === 0}
                            onClick={() => move(r.id, -1)}
                          />
                          <Button
                            label="↓"
                            variant="ghost"
                            size="sm"
                            isDisabled={i === rules.length - 1}
                            onClick={() => move(r.id, 1)}
                          />
                          <IconButton
                            label={`Remove rule ${i + 1}`}
                            variant="ghost"
                            size="sm"
                            icon={<Icon icon={IconTrash} size="sm" />}
                            onClick={() => removeRule(r.id)}
                          />
                        </HStack>
                        {i < rules.length - 1 && <Divider />}
                      </VStack>
                    ))}
                  </VStack>
                )}

                <Divider />
                <span className="eyebrow">Add rule</span>
                <Grid columns={{ minWidth: 160, repeat: "fit" }} gap={3}>
                  <Selector
                    label="Applies to"
                    value={scopeType}
                    onChange={(v) => {
                      setScopeType(v as RuleScope);
                      setScopeValue("");
                    }}
                    options={SCOPES}
                  />
                  {scopeType === "tag" ? (
                    <TextInput label="Tag" value={scopeValue} onChange={setScopeValue} placeholder="prod" />
                  ) : (
                    <Selector
                      label="Target"
                      value={scopeValue}
                      onChange={setScopeValue}
                      placeholder="Select…"
                      hasSearch={scopeOptions.length > 8}
                      options={scopeOptions}
                    />
                  )}
                  <Selector
                    label="Capture"
                    value={draftCapture}
                    onChange={(v) => setDraftCapture(v as Capture)}
                    options={[
                      { value: "meta", label: "Metadata only" },
                      { value: "content", label: "Full content" },
                    ]}
                  />
                </Grid>

                <Slider
                  label={`Sample rate — ${draftSample}%`}
                  value={draftSample}
                  onChange={setDraftSample}
                  min={1}
                  max={100}
                  step={1}
                />

                {preview.data?.preview && (
                  <Text type="supporting" color="secondary">
                    This rule currently matches ~{int(preview.data.preview.matching_rpm)} requests/min.
                  </Text>
                )}

                <HStack hAlign="end">
                  <Button
                    label="Add rule"
                    variant="secondary"
                    icon={<Icon icon={IconPlus} size="sm" />}
                    onClick={addRule}
                    isDisabled={!scopeValue.trim()}
                  />
                </HStack>
              </VStack>
            )}

            {/* Pending changes: what will change, and how much traffic it touches. */}
            {changes.length > 0 && (
              <>
                <Divider />
                <VStack gap={3}>
                  <Banner
                    status="warning"
                    title={`${changes.length} unsaved change${changes.length > 1 ? "s" : ""}`}
                    description={
                      mode === "off"
                        ? "Nothing will be exported once applied."
                        : affected < 1
                          ? `No traffic in the last minute to size this against. ${
                              mode === "everything"
                                ? captureContent
                                  ? "All requests will be exported, including prompt content."
                                  : "All requests will be exported, metadata only."
                                : "Only traffic matching a rule will be exported."
                            }`
                          : `Once applied, ~${int(affected)} requests/min will be exported${
                              mode === "everything" && captureContent ? ", including prompt content" : ""
                            }.`
                    }
                  />
                  <VStack gap={1}>
                    {changes.map((c) => (
                      <Text key={c} type="supporting" color="secondary">
                        · {c}
                      </Text>
                    ))}
                  </VStack>
                  <HStack gap={2} hAlign="end">
                    <Button label="Discard" variant="ghost" onClick={discardPolicy} />
                    <Button
                      label="Apply policy"
                      variant="primary"
                      isLoading={save.isPending}
                      onClick={savePolicy}
                    />
                  </HStack>
                </VStack>
              </>
            )}
          </VStack>
        </Card>

        {/* Live export feed */}
        <Card padding={0}>
          <VStack gap={0}>
            <HStack padding={4} hAlign="between" vAlign="center">
              <Heading level={4}>Exported events</Heading>
              <Text type="supporting" color="secondary">
                Live · refreshes every 5s
              </Text>
            </HStack>
            <Divider />
            {exported.isLoading ? (
              <Loading />
            ) : (exported.data?.length ?? 0) === 0 ? (
              <Empty
                title="Nothing exported yet"
                description="Events appear here once traffic the capture policy lets out is exported to a destination."
              />
            ) : (
              <Table
                data={(exported.data ?? []) as Array<ExportedEvent & Record<string, unknown>>}
                columns={eventColumns}
                idKey={(e) => `${e.ts}-${e.key_name}-${e.model}`}
                density="compact"
                dividers="rows"
                hasHover
                textOverflow="truncate"
              />
            )}
          </VStack>
        </Card>
      </VStack>
    </Page>
  );
}
