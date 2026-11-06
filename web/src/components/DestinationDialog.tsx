import { useMemo, useState } from "react";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Divider } from "@astryxdesign/core/Divider";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { TextInput } from "@astryxdesign/core/TextInput";
import { MultiSelector } from "@astryxdesign/core/MultiSelector";
import { SelectableCard } from "@astryxdesign/core/SelectableCard";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { useToast } from "@astryxdesign/core/Toast";

import type {
  ContentMode,
  DestinationInput,
  DestinationScope,
  DestinationTestResult,
  ScopeMode,
  TelemetryDestination,
  TelemetryKind,
} from "../lib/types";
import {
  useCreateDestination,
  useGatewayGroups,
  useTeams,
  useTestDestination,
  useUpdateDestination,
} from "../lib/hooks";
import { FormActions, FormDialog, Hint } from "./dialogs";
import { IconPlus, IconTrash } from "./icons";

/** Scope value for traffic the control plane serves itself. */
export const CONTROL_PLANE = "_control_plane";

// ── Kind-specific fields ────────────────────────────────────────
// Everything a kind needs beyond name, content and scope. A field writes to
// the destination's endpoint, traces_url, a config key or a secret. A kind
// missing here still gets endpoint, traces URL and one input per secret
// field from /api/telemetry/kinds, so adding a kind needs no UI change to
// work, only to read well.
interface FieldSpec {
  source: "endpoint" | "traces_url" | "config" | "secret";
  /** Config or secret key. */
  key?: string;
  label: string;
  placeholder?: string;
  description?: string;
  isRequired?: boolean;
  isAdvanced?: boolean;
}

interface KindSpec {
  fields: FieldSpec[];
  /** Where the headers editor sits. */
  headers: "main" | "advanced";
  headerHint?: string;
}

const TRACES_URL_HINT = "Full URL that receives spans, when it is not the default path under the endpoint";

const KIND_SPECS: Record<string, KindSpec> = {
  otlp: {
    fields: [
      {
        source: "endpoint",
        label: "Endpoint",
        placeholder: "https://otel-collector.example.com:4318",
        description: "OTLP/HTTP base URL. Spans are sent to {endpoint}/v1/traces.",
        isRequired: true,
      },
      {
        source: "traces_url",
        label: "Traces URL",
        placeholder: "https://collector.example.com/otlp/v1/traces",
        description: TRACES_URL_HINT,
        isAdvanced: true,
      },
      {
        source: "config",
        key: "trace_link",
        label: "Trace link",
        placeholder: "https://grafana.example.com/explore?…{trace_id}…",
        description: "Opens a request's trace from the Requests page. Must contain {trace_id}.",
        isAdvanced: true,
      },
    ],
    headers: "main",
    headerHint: "Sent with every export, for example Authorization or X-Scope-OrgID.",
  },
  langfuse: {
    fields: [
      {
        source: "endpoint",
        label: "Host",
        placeholder: "https://cloud.langfuse.com",
        isRequired: true,
      },
      { source: "config", key: "public_key", label: "Public key", placeholder: "pk-lf-…", isRequired: true },
      { source: "secret", key: "secret_key", label: "Secret key", placeholder: "sk-lf-…", isRequired: true },
      {
        source: "config",
        key: "ui_url",
        label: "UI URL",
        placeholder: "https://langfuse.example.com",
        description: "Where browsers open Langfuse, when the host above is an internal name",
      },
      {
        source: "traces_url",
        label: "Traces URL",
        placeholder: "https://cloud.langfuse.com/api/public/otel/v1/traces",
        description: TRACES_URL_HINT,
        isAdvanced: true,
      },
    ],
    headers: "advanced",
    headerHint: "Authorization is set from the key pair.",
  },
};

function specFor(kind: TelemetryKind | undefined): KindSpec {
  const known = kind && KIND_SPECS[kind.name];
  if (known) return known;
  return {
    fields: [
      { source: "endpoint", label: "Endpoint", isRequired: true },
      ...(kind?.secret_fields ?? []).map(
        (s): FieldSpec => ({ source: "secret", key: s, label: s.replace(/_/g, " "), isRequired: true }),
      ),
      { source: "traces_url", label: "Traces URL", description: TRACES_URL_HINT, isAdvanced: true },
    ],
    headers: "advanced",
  };
}

// ── Headers editor ──────────────────────────────────────────────
// Values are write-only: a stored header comes back as a name, its value
// stays on the server unless replaced here. Sent as the complete set, so a
// removed row deletes the header.
interface HeaderRow {
  id: string;
  name: string;
  value: string;
  /** Saved on the server; a blank value keeps it. */
  isStored: boolean;
}

function HeadersEditor({
  rows,
  onChange,
  hint,
}: {
  rows: HeaderRow[];
  onChange: (rows: HeaderRow[]) => void;
  hint?: string;
}) {
  const patch = (id: string, p: Partial<HeaderRow>) => onChange(rows.map((r) => (r.id === id ? { ...r, ...p } : r)));
  return (
    <VStack gap={2}>
      <VStack gap={0}>
        <Text type="body" weight="medium">
          Headers
        </Text>
        <Hint>{hint ? `${hint} Values are write-only.` : "Values are write-only."}</Hint>
      </VStack>
      {rows.map((r, i) => (
        <HStack key={r.id} gap={2} vAlign="start">
          <div style={{ flex: 1 }}>
            <TextInput
              label={`Header ${i + 1} name`}
              isLabelHidden
              value={r.name}
              onChange={(v) => patch(r.id, { name: v })}
              placeholder="Authorization"
              isDisabled={r.isStored}
            />
          </div>
          <div style={{ flex: 1 }}>
            <TextInput
              label={`Header ${i + 1} value`}
              isLabelHidden
              type="password"
              value={r.value}
              onChange={(v) => patch(r.id, { value: v })}
              placeholder={r.isStored ? "unchanged" : "Value"}
              status={
                !r.isStored && r.name.trim() && !r.value ? { type: "error", message: "Enter a value" } : undefined
              }
            />
          </div>
          <IconButton
            label={`Remove header ${r.name || i + 1}`}
            variant="ghost"
            size="md"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={() => onChange(rows.filter((x) => x.id !== r.id))}
          />
        </HStack>
      ))}
      <HStack>
        <Button
          label="Add header"
          variant="ghost"
          size="sm"
          icon={<Icon icon={IconPlus} size="sm" />}
          onClick={() => onChange([...rows, { id: crypto.randomUUID(), name: "", value: "", isStored: false }])}
        />
      </HStack>
    </VStack>
  );
}

// ── Scope ───────────────────────────────────────────────────────
function ScopeRow({
  label,
  noun,
  value,
  onChange,
  options,
}: {
  label: string;
  noun: string;
  value: DestinationScope;
  onChange: (v: DestinationScope) => void;
  options: { value: string; label: string }[];
}) {
  // A scope can name something the list no longer has; keep it visible so
  // saving does not silently drop it.
  const all = useMemo(
    () => [
      ...options,
      ...value.values.filter((v) => !options.some((o) => o.value === v)).map((v) => ({ value: v, label: v })),
    ],
    [options, value.values],
  );
  return (
    <VStack gap={2}>
      <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
        <Text type="body" weight="medium">
          {label}
        </Text>
        <SegmentedControl
          label={`${label} scope`}
          size="sm"
          value={value.mode}
          onChange={(m) => onChange({ ...value, mode: m as ScopeMode })}
        >
          <SegmentedControlItem value="all" label="All" />
          <SegmentedControlItem value="selected" label="Selected" />
          <SegmentedControlItem value="except" label="All except" />
        </SegmentedControl>
      </HStack>
      {value.mode !== "all" && (
        <MultiSelector
          label={value.mode === "selected" ? `${label} to export` : `${label} to leave out`}
          isLabelHidden
          value={value.values}
          onChange={(v) => onChange({ ...value, values: v })}
          options={all}
          placeholder={`Choose ${noun}s…`}
          triggerDisplay="badges"
          hasSearch={all.length > 8}
          status={
            value.mode === "selected" && value.values.length === 0
              ? { type: "warning", message: `No ${noun}s selected: nothing is exported` }
              : undefined
          }
        />
      )}
    </VStack>
  );
}

// ── Dialog ──────────────────────────────────────────────────────
const KIB = 1024;

export function DestinationDialog({
  editing,
  kinds,
  policyCapturesContent,
  onClose,
}: {
  editing: TelemetryDestination | null;
  kinds: TelemetryKind[];
  /** Whether the capture policy can let content out at all. */
  policyCapturesContent: boolean;
  onClose: () => void;
}) {
  const create = useCreateDestination();
  const update = useUpdateDestination();
  const test = useTestDestination();
  const teams = useTeams();
  const groups = useGatewayGroups();
  const showToast = useToast();

  const [name, setName] = useState(editing?.name ?? "");
  const [kindName, setKindName] = useState(editing?.kind ?? kinds[0]?.name ?? "");
  const [endpoint, setEndpoint] = useState(editing?.endpoint ?? "");
  const [tracesUrl, setTracesUrl] = useState(editing?.traces_url ?? "");
  const [config, setConfig] = useState<Record<string, string>>(editing?.config ?? {});
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [headers, setHeaders] = useState<HeaderRow[]>(
    (editing?.headers ?? []).map((h) => ({ id: crypto.randomUUID(), name: h, value: "", isStored: true })),
  );
  const [contentMode, setContentMode] = useState<ContentMode>(editing?.content_mode ?? "metadata_only");
  const [maxKib, setMaxKib] = useState(String(Math.round((editing?.max_content_bytes || 64 * KIB) / KIB)));
  const [teamScope, setTeamScope] = useState<DestinationScope>(editing?.team_scope ?? { mode: "all", values: [] });
  const [groupScope, setGroupScope] = useState<DestinationScope>(
    editing?.group_scope ?? { mode: "all", values: [] },
  );
  const [result, setResult] = useState<DestinationTestResult | null>(null);

  const kind = kinds.find((k) => k.name === kindName);
  const spec = specFor(kind);
  const stored = new Set(editing?.secret_names ?? []);

  const get = (f: FieldSpec) =>
    f.source === "endpoint"
      ? endpoint
      : f.source === "traces_url"
        ? tracesUrl
        : f.source === "config"
          ? (config[f.key!] ?? "")
          : (secrets[f.key!] ?? "");
  const set = (f: FieldSpec, v: string) => {
    setResult(null);
    if (f.source === "endpoint") setEndpoint(v);
    else if (f.source === "traces_url") setTracesUrl(v);
    else if (f.source === "config") setConfig({ ...config, [f.key!]: v });
    else setSecrets({ ...secrets, [f.key!]: v });
  };

  // A required field is met by a value, a stored secret, or (for an OTLP
  // endpoint) a full traces URL.
  const missing = spec.fields.filter((f) => {
    if (!f.isRequired || get(f).trim()) return false;
    if (f.source === "secret" && stored.has(f.key!)) return false;
    if (f.source === "endpoint" && kindName === "otlp" && tracesUrl.trim()) return false;
    return true;
  });
  const maxBytes = Number(maxKib) * KIB;
  const badMax = contentMode === "content" && (!Number.isInteger(Number(maxKib)) || maxBytes < KIB || maxBytes > KIB * KIB);
  const badHeader = headers.some((h) => !h.name.trim() || (!h.isStored && !h.value));
  const trace = config.trace_link ?? "";
  const badLink = kindName === "otlp" && trace.trim() !== "" && !trace.includes("{trace_id}");
  const invalid = !name.trim() || !kind || missing.length > 0 || badMax || badHeader || badLink;

  function body(): DestinationInput {
    const cfg: Record<string, string> = {};
    // Only the active kind's config keys; others would be refused.
    for (const f of spec.fields) if (f.source === "config" && config[f.key!]?.trim()) cfg[f.key!] = config[f.key!].trim();
    const sec: Record<string, string> = {};
    for (const s of kind?.secret_fields ?? []) if (secrets[s]) sec[s] = secrets[s];
    return {
      name: name.trim(),
      kind: kindName,
      enabled: editing?.enabled ?? true,
      endpoint: endpoint.trim(),
      traces_url: tracesUrl.trim(),
      config: cfg,
      secrets: sec,
      headers: headers.map((h) => (h.value ? { name: h.name.trim(), value: h.value } : { name: h.name.trim() })),
      content_mode: contentMode,
      max_content_bytes: contentMode === "content" ? maxBytes : (editing?.max_content_bytes ?? 0),
      team_scope: teamScope,
      group_scope: groupScope,
    };
  }

  async function runTest() {
    setResult(null);
    try {
      // With an id the server fills in stored secrets and header values.
      setResult(await test.mutateAsync(editing ? { ...body(), id: editing.id } : body()));
    } catch (err) {
      setResult({ ok: false, detail: err instanceof Error ? err.message : "Test failed" });
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (invalid) return;
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...body() });
        showToast({ body: "Destination saved" });
      } else {
        await create.mutateAsync(body());
        showToast({ body: "Destination added" });
      }
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  const field = (f: FieldSpec) => {
    const isSecret = f.source === "secret";
    const isSet = isSecret && stored.has(f.key!);
    return (
      <TextInput
        key={`${f.source}:${f.key ?? ""}`}
        label={f.label}
        type={isSecret ? "password" : "text"}
        value={get(f)}
        onChange={(v) => set(f, v)}
        placeholder={isSet ? "set (unchanged)" : f.placeholder}
        description={isSet ? "A value is stored. Leave blank to keep it." : f.description}
        isRequired={f.isRequired && !isSet}
        isOptional={!f.isRequired}
        status={
          f.source === "config" && f.key === "trace_link" && badLink
            ? { type: "error", message: "Must contain {trace_id}" }
            : undefined
        }
      />
    );
  };

  const main = spec.fields.filter((f) => !f.isAdvanced);
  const advanced = spec.fields.filter((f) => f.isAdvanced);
  const headersEditor = (
    <HeadersEditor
      rows={headers}
      onChange={(r) => {
        setResult(null);
        setHeaders(r);
      }}
      hint={spec.headerHint}
    />
  );
  const advancedOpen =
    advanced.some((f) => get(f).trim() !== "") || (spec.headers === "advanced" && headers.length > 0);

  const teamOptions = (teams.data ?? []).map((t) => ({ value: t.id, label: t.name }));
  const groupOptions = [
    { value: CONTROL_PLANE, label: "Control plane" },
    ...(groups.data ?? []).map((g) => ({ value: g.id, label: g.region ? `${g.name} · ${g.region}` : g.name })),
  ];

  const busy = create.isPending || update.isPending;

  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={editing ? `Edit ${editing.name}` : "Add destination"}
      subtitle="Where request traces are exported, and which of them."
      wide
    >
      <form onSubmit={submit} noValidate>
        <VStack gap={4}>
          {editing?.error && (
            <Banner
              status="error"
              title="This destination can't run"
              description={`${editing.error}. Enter every secret and header value again to save it.`}
            />
          )}
          <TextInput label="Name" value={name} onChange={setName} placeholder="Grafana Tempo" isRequired />

          <VStack gap={2}>
            <Text type="body" weight="medium">
              Kind
            </Text>
            <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
              {kinds.map((k) => (
                <SelectableCard
                  key={k.name}
                  label={k.label}
                  isSelected={kindName === k.name}
                  isDisabled={!!editing && editing.kind !== k.name}
                  onChange={() => {
                    setKindName(k.name);
                    setResult(null);
                  }}
                  padding={3}
                >
                  <HStack gap={2} vAlign="center" hAlign="between">
                    <Text type="body" weight="medium">
                      {k.label}
                    </Text>
                    {k.beta && <Badge variant="info" label="Beta" />}
                  </HStack>
                </SelectableCard>
              ))}
            </Grid>
            {editing && <Hint>The kind can't be changed. Add a new destination instead.</Hint>}
          </VStack>

          {main.map(field)}
          {spec.headers === "main" && headersEditor}

          {(advanced.length > 0 || spec.headers === "advanced") && (
            <Collapsible
              defaultIsOpen={advancedOpen}
              trigger={
                <Text type="body" weight="medium">
                  Advanced
                </Text>
              }
            >
              <VStack gap={4} paddingBlock={2}>
                {advanced.map(field)}
                {spec.headers === "advanced" && headersEditor}
              </VStack>
            </Collapsible>
          )}

          <Divider />

          <VStack gap={2}>
            <Text type="body" weight="medium">
              Content
            </Text>
            <Grid columns={{ minWidth: 220, repeat: "fit" }} gap={3}>
              <SelectableCard
                label="Metadata only"
                isSelected={contentMode === "metadata_only"}
                onChange={() => setContentMode("metadata_only")}
                padding={3}
              >
                <VStack gap={1}>
                  <Text type="body" weight="medium">
                    Metadata only
                  </Text>
                  <Hint>Model, tokens, cost, latency and status. No prompts or completions.</Hint>
                </VStack>
              </SelectableCard>
              <SelectableCard
                label="Full content"
                isSelected={contentMode === "content"}
                onChange={() => setContentMode("content")}
                padding={3}
              >
                <VStack gap={1}>
                  <Text type="body" weight="medium">
                    Full content
                  </Text>
                  <Hint>Also prompts, system prompts and completions, where the capture policy allows it.</Hint>
                </VStack>
              </SelectableCard>
            </Grid>
            {contentMode === "content" && (
              <>
                <TextInput
                  label="Content cap (KiB)"
                  value={maxKib}
                  onChange={(v) => setMaxKib(v.replace(/[^0-9]/g, ""))}
                  description="Longer content is cut and the span marked truncated. 1 to 1024."
                  status={badMax ? { type: "error", message: "Enter 1 to 1024" } : undefined}
                />
                {!policyCapturesContent && (
                  <Banner
                    status="info"
                    title="The capture policy exports metadata only"
                    description="Content reaches this destination only for traffic the capture policy captures with content."
                  />
                )}
              </>
            )}
          </VStack>

          <Divider />

          <VStack gap={3}>
            <VStack gap={0}>
              <Text type="body" weight="medium">
                Scope
              </Text>
              <Hint>Traffic must match both. Gateway groups set to never export are left out whatever this says.</Hint>
            </VStack>
            <ScopeRow label="Teams" noun="team" value={teamScope} onChange={setTeamScope} options={teamOptions} />
            <ScopeRow
              label="Gateway groups"
              noun="group"
              value={groupScope}
              onChange={setGroupScope}
              options={groupOptions}
            />
          </VStack>

          {result && (
            <Banner
              status={result.ok ? "success" : "error"}
              title={result.ok ? "Test span accepted" : result.status ? `Test failed (HTTP ${result.status})` : "Test failed"}
              description={result.detail}
              endContent={
                result.trace_url ? (
                  <a href={result.trace_url} target="_blank" rel="noreferrer" className="link">
                    <Text type="supporting">Open the test trace</Text>
                  </a>
                ) : undefined
              }
            />
          )}

          <Divider />
          <HStack hAlign="between" vAlign="center" gap={2} wrap="wrap">
            <Button
              label="Test"
              variant="secondary"
              isLoading={test.isPending}
              isDisabled={invalid}
              onClick={runTest}
            />
            <FormActions
              onCancel={onClose}
              submitLabel={editing ? "Save changes" : "Add destination"}
              isLoading={busy}
              isDisabled={invalid}
            />
          </HStack>
        </VStack>
      </form>
    </FormDialog>
  );
}
