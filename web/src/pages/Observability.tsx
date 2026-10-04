import { useEffect, useMemo, useRef, useState } from "react";
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
import { useToast } from "@astryxdesign/core/Toast";

import type { Capture, ExportedEvent, RuleScope, TelemetryMode, TelemetryRule } from "../lib/types";
import {
  useKeys,
  useObservabilityExported,
  useObservabilityRuleStats,
  useSetTelemetry,
  useTeams,
  useTelemetry,
  useTestObservability,
  useUsers,
} from "../lib/hooks";
import { int, money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { IconPlus, IconTrash } from "../components/icons";

const SCOPES: { value: RuleScope; label: string }[] = [
  { value: "team", label: "Team" },
  { value: "user", label: "User" },
  { value: "key", label: "API key" },
  { value: "tag", label: "Tag" },
];

export default function Observability() {
  const tel = useTelemetry();
  const save = useSetTelemetry();
  const test = useTestObservability();
  const teams = useTeams();
  const users = useUsers();
  const keys = useKeys();
  const exported = useObservabilityExported();
  const stats = useObservabilityRuleStats();
  const showToast = useToast();

  // ── Destination ──────────────────────────────────────────────
  const [host, setHost] = useState("");
  const [publicKey, setPublicKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [enabled, setEnabled] = useState(false);
  const [captureContent, setCaptureContent] = useState(false);
  const [mode, setMode] = useState<TelemetryMode>("everything");
  const [rules, setRules] = useState<TelemetryRule[]>([]);

  // Hydrate once; secret_key always returns empty from the server.
  const hydrated = useRef(false);
  useEffect(() => {
    const t = tel.data?.telemetry;
    if (!t || hydrated.current) return;
    hydrated.current = true;
    setHost(t.host);
    setPublicKey(t.public_key);
    setEnabled(t.enabled);
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

  function blob(over: Record<string, unknown> = {}, rulesOver?: TelemetryRule[]) {
    return {
      type: "langfuse",
      host: host.trim(),
      public_key: publicKey.trim(),
      secret_key: secretKey, // blank keeps the stored secret
      enabled,
      capture_content: captureContent,
      mode,
      rules: rulesOver ?? rules,
      ...over,
    };
  }


  async function saveDestination() {
    try {
      // Destination only: the capture policy has its own explicit Apply,
      // because it decides whether prompt content leaves the gateway.
      await save.mutateAsync(
        blob(
          saved ? { mode: saved.mode || "everything", capture_content: saved.capture_content } : {},
          saved ? [...saved.rules] : undefined,
        ),
      );
      setSecretKey("");
      showToast({ body: "Telemetry settings saved" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  async function testConnection() {
    try {
      const res = await test.mutateAsync(blob());
      showToast({ body: res.detail || (res.ok ? "Connection OK" : "Connection failed"), type: res.ok ? "info" : "error" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

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
  const health = tel.data?.health;

  // ── Pending policy changes ───────────────────────────────────
  // Capture policy decides whether prompts and completions leave the gateway,
  // so it is edited as a draft and applied explicitly: a mis-click here is a
  // data-egress change, not a display preference.
  const saved = tel.data?.telemetry;
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
      await save.mutateAsync(blob());
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
      description="Export request traces to Langfuse. Rules decide which traffic is captured and whether prompt content leaves the gateway."
    >
      <VStack gap={4}>
        {/* Delivery health. The live feed below lists events *selected* for
            export, which looks identical whether Langfuse accepted them or
            rejected every one, so failures have to be stated outright. */}
        {health && health.dropped > 0 && (
          <Banner
            status="error"
            title={`Langfuse rejected ${int(health.dropped)} event${health.dropped > 1 ? "s" : ""}`}
            description={`${health.last_error || "export failed"}${
              health.delivered > 0 ? ` — ${int(health.delivered)} delivered successfully.` : " — nothing has been delivered."
            } Events listed below were selected for export but did not arrive. Use Test connection to check the host and key pair.`}
          />
        )}
        {health && health.dropped === 0 && health.delivered > 0 && (
          <Banner
            status="success"
            title={`${int(health.delivered)} event${health.delivered === 1 ? "" : "s"} delivered to Langfuse`}
            description={health.last_ok_at ? `Last successful export at ${new Date(health.last_ok_at).toLocaleTimeString()}.` : undefined}
          />
        )}
        {enabled && mode === "by_rule" && rules.length === 0 && changes.length === 0 && (
          <Banner
            status="warning"
            title="Export is on, but no rules exist"
            description="In by-rule mode traffic matching nothing is not exported, so nothing is reaching Langfuse. Add a rule for a team, user or key below, or switch to Everything."
          />
        )}

        <Grid columns={{ minWidth: 380, repeat: "fit" }} gap={4}>
          {/* Destination */}
          <Card>
            <VStack gap={4}>
              <HStack hAlign="between" vAlign="center">
                <Heading level={4}>Destination</Heading>
                <HStack gap={2} vAlign="center">
                  <StatusDot
                    variant={enabled ? "success" : "neutral"}
                    label={enabled ? "Export enabled" : "Export disabled"}
                  />
                  <Badge variant="neutral" label="langfuse" />
                </HStack>
              </HStack>

              <TextInput
                label="Host"
                value={host}
                onChange={setHost}
                placeholder="https://cloud.langfuse.com"
                isRequired
              />
              <TextInput label="Public key" value={publicKey} onChange={setPublicKey} placeholder="pk-lf-…" isRequired />
              <TextInput
                label="Secret key"
                type="password"
                value={secretKey}
                onChange={setSecretKey}
                placeholder={tel.data?.configured ? "•••••••• (leave blank to keep)" : "sk-lf-…"}
              />

              <Switch
                label="Enable export"
                description="Ship trace events to the destination above"
                value={enabled}
                onChange={setEnabled}
              />

              <HStack gap={2} hAlign="end" wrap="wrap">
                <Button
                  label="Test connection"
                  variant="secondary"
                  isLoading={test.isPending}
                  onClick={testConnection}
                  isDisabled={!host.trim()}
                />
                <Button
                  label="Save"
                  variant="primary"
                  isLoading={save.isPending}
                  onClick={saveDestination}
                  isDisabled={!host.trim() || !publicKey.trim()}
                />
              </HStack>
            </VStack>
          </Card>

          {/* Capture policy */}
          <Card>
            <VStack gap={4}>
              <Heading level={4}>Capture policy</Heading>

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
                  description="Off exports metadata only — no prompts or completions leave the gateway"
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
        </Grid>

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
                description="Once traffic matches your capture policy, events appear here."
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
