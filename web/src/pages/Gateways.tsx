import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { StatusDot } from "@astryxdesign/core/StatusDot";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { Divider } from "@astryxdesign/core/Divider";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { MultiSelector } from "@astryxdesign/core/MultiSelector";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { DataRule, GatewayGroup, Node } from "../lib/types";
import {
  useCreateGatewayGroup,
  useDeleteGatewayGroup,
  useGatewayGroups,
  useNodes,
  useRotateGatewayToken,
  useTeams,
  useUpdateGatewayGroup,
} from "../lib/hooks";
import { int } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog, SecretDialog } from "../components/dialogs";
import { IconEdit, IconKey, IconPlus, IconTrash } from "../components/icons";

function uptime(seconds: number): string {
  if (!seconds) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}

// ── Live cluster (self + peers) ─────────────────────────────────
function ClusterTable() {
  const nodes = useNodes();

  const columns: TableColumn<Node & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Node",
      width: proportional(1.3),
      renderCell: (n) => (
        <HStack gap={2} vAlign="center">
          <StatusDot
            variant={n.status === "ok" ? "success" : n.status === "warn" ? "warning" : "error"}
            label={`${n.name} ${n.status}`}
            isPulsing={n.status !== "ok"}
          />
          <VStack gap={0}>
            <HStack gap={1.5} vAlign="center">
              <Text type="body" weight="medium" maxLines={1}>
                {n.name}
              </Text>
              {n.role === "leader" && <Badge variant="purple" label="leader" />}
              {n.role === "gateway" && <Badge variant="neutral" label={n.region ? `${n.group} · ${n.region}` : (n.group ?? "gateway")} />}
            </HStack>
            {n.addr && <Code>{n.addr}</Code>}
          </VStack>
        </HStack>
      ),
    },
    {
      key: "version",
      header: "Version",
      width: pixel(130),
      renderCell: (n) => (
        <HStack gap={1.5} vAlign="center">
          <Text type="supporting">{n.version || "—"}</Text>
          {n.version_skew && <Badge variant="warning" label="skew" />}
        </HStack>
      ),
    },
    { key: "rps", header: "RPS", width: pixel(80), align: "end", renderCell: (n) => int(n.rps) },
    {
      key: "p50_ms",
      header: "p50",
      width: pixel(100),
      align: "end",
      renderCell: (n) => `${int(n.p50_ms)} ms`,
    },
    {
      key: "cpu_pct",
      header: "CPU",
      width: pixel(110),
      renderCell: (n) => (
        <ProgressBar value={n.cpu_pct} max={100} label={`CPU ${Math.round(n.cpu_pct)}%`} isLabelHidden />
      ),
    },
    {
      key: "mem_pct",
      header: "Memory",
      width: pixel(110),
      renderCell: (n) => (
        <ProgressBar value={n.mem_pct} max={100} label={`Memory ${Math.round(n.mem_pct)}%`} isLabelHidden />
      ),
    },
    {
      key: "uptime_s",
      header: "Uptime",
      width: pixel(96),
      align: "end",
      renderCell: (n) => uptime(n.uptime_s),
    },
  ];

  return (
    <Card padding={0}>
      <VStack gap={0}>
        <HStack padding={4} hAlign="between" vAlign="center">
          <Heading level={4}>Cluster</Heading>
          <Text type="supporting" color="secondary">
            Refreshes automatically
          </Text>
        </HStack>
        <Divider />
        {nodes.isLoading ? (
          <Loading />
        ) : nodes.error ? (
          <ErrorState error={nodes.error} onRetry={() => nodes.refetch()} />
        ) : (nodes.data?.nodes.length ?? 0) === 0 ? (
          <Empty title="No nodes reporting" description="The control plane has not heard from any node yet." />
        ) : (
          <Table
            data={(nodes.data?.nodes ?? []) as Array<Node & Record<string, unknown>>}
            columns={columns}
            idKey="id"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </VStack>
    </Card>
  );
}

function ClusterEvents() {
  const nodes = useNodes();
  const events = nodes.data?.cluster_events ?? [];
  return (
    <Card>
      <VStack gap={3}>
        <Heading level={4}>Cluster events</Heading>
        {events.length === 0 ? (
          <Text type="supporting" color="secondary">
            No recent events.
          </Text>
        ) : (
          <VStack gap={2}>
            {events.slice(0, 12).map((e, i) => (
              <HStack key={i} gap={2} vAlign="start">
                <StatusDot
                  variant={e.level === "err" ? "error" : e.level === "warn" ? "warning" : "neutral"}
                  label={e.level}
                />
                <VStack gap={0}>
                  <Text type="supporting">{e.text}</Text>
                  <Timestamp value={e.ts} format="relative" isLive />
                </VStack>
              </HStack>
            ))}
          </VStack>
        )}
      </VStack>
    </Card>
  );
}

// ── Gateway groups ──────────────────────────────────────────────
const DATA_RULES: { value: DataRule; label: string }[] = [
  { value: "inherit", label: "Follow the telemetry settings" },
  { value: "no_content", label: "Never capture prompts or responses" },
  { value: "no_export", label: "Never export" },
];

function GroupForm({
  group,
  onClose,
  onToken,
}: {
  group?: GatewayGroup;
  onClose: () => void;
  onToken: (t: string) => void;
}) {
  const teams = useTeams();
  const create = useCreateGatewayGroup();
  const update = useUpdateGatewayGroup();
  const showToast = useToast();
  const [name, setName] = useState(group?.name ?? "");
  const [region, setRegion] = useState(group?.region ?? "");
  const [allowed, setAllowed] = useState<string[]>(group?.allowed_teams ?? []);
  const [rule, setRule] = useState<DataRule>(group?.data_rule ?? "inherit");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const body = {
      name: name.trim(),
      region: region.trim(),
      allowed_teams: allowed.length ? allowed : null,
      data_rule: rule,
    };
    try {
      if (group) {
        await update.mutateAsync({ id: group.id, ...body });
        showToast({ body: "Gateway group saved" });
      } else {
        const res = (await create.mutateAsync(body)) as { join_token: string };
        onToken(res.join_token);
      }
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  const teamOptions = (teams.data ?? []).map((t) => ({ value: t.id, label: t.name }));
  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          placeholder="eu-west"
          description="Shown here and on each gateway's requests"
          isRequired
          hasAutoFocus
        />
        <TextInput
          label="Region"
          value={region}
          onChange={setRegion}
          placeholder="eu"
          description="Gateways try deployments with the same region first. Empty: no preference."
        />
        <MultiSelector
          label="Teams"
          value={allowed}
          onChange={setAllowed}
          options={teamOptions}
          placeholder="Every team"
          triggerDisplay="badges"
          hasSearch={teamOptions.length > 8}
          description="Only these teams' keys, and their members' own keys, work on these gateways. None selected: every team."
        />
        <Selector
          label="Telemetry"
          value={rule}
          onChange={(v) => setRule(v as DataRule)}
          options={DATA_RULES}
          description="What requests through these gateways may send to the observability exporter."
        />
        <FormActions
          onCancel={onClose}
          submitLabel={group ? "Save" : "Create group"}
          isLoading={create.isPending || update.isPending}
          isDisabled={!name.trim()}
        />
      </VStack>
    </form>
  );
}

function JoinTokenDialog({ token, onClose }: { token: string; onClose: () => void }) {
  const run = `docker run -p 8080:8080 \\
  -e LLMR_CONTROL_PLANE_URL=${window.location.origin} \\
  -e LLMR_JOIN_TOKEN=${token} \\
  ghcr.io/exitcodenihil/llm-router-gateway`;
  return (
    <SecretDialog secret={token} onClose={onClose} title="Gateway group ready" noun="join token">
      <VStack gap={2}>
        <Text type="supporting" color="secondary">
          Start as many gateways as you like with this token. Each one joins the group, takes its config from here and
          reports its usage back.
        </Text>
        <CodeBlock code={run} language="bash" width="100%" isWrapped />
      </VStack>
    </SecretDialog>
  );
}

function GatewayGroups({ onNew }: { onNew: () => void }) {
  const groups = useGatewayGroups();
  const teams = useTeams();
  const rotate = useRotateGatewayToken();
  const del = useDeleteGatewayGroup();
  const showToast = useToast();
  const [editing, setEditing] = useState<GatewayGroup | null>(null);
  const [rotating, setRotating] = useState<GatewayGroup | null>(null);
  const [deleting, setDeleting] = useState<GatewayGroup | null>(null);
  const [token, setToken] = useState<string | null>(null);

  const teamName = (id: string) => teams.data?.find((t) => t.id === id)?.name ?? id.slice(0, 8);
  const columns: TableColumn<GatewayGroup & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Group",
      width: proportional(1.2),
      renderCell: (g) => (
        <HStack gap={1.5} vAlign="center">
          <Text type="body" weight="medium" maxLines={1}>
            {g.name}
          </Text>
          {g.region && <Badge variant="neutral" label={g.region} />}
        </HStack>
      ),
    },
    {
      key: "allowed_teams",
      header: "Teams",
      width: proportional(1.4),
      renderCell: (g) => (
        <Text type="supporting" maxLines={1}>
          {g.allowed_teams?.length ? g.allowed_teams.map(teamName).join(", ") : "Every team"}
        </Text>
      ),
    },
    {
      key: "data_rule",
      header: "Telemetry",
      width: pixel(150),
      renderCell: (g) =>
        g.data_rule === "inherit" ? (
          <Text type="supporting" color="secondary">
            Settings
          </Text>
        ) : (
          <Badge variant="warning" label={g.data_rule === "no_export" ? "never exported" : "no content"} />
        ),
    },
    {
      key: "live_instances",
      header: "Running",
      width: pixel(90),
      align: "end",
      renderCell: (g) => int(g.live_instances),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(124),
      align: "end",
      renderCell: (g) => (
        <HStack gap={0.5} hAlign="end">
          <IconButton label={`Edit ${g.name}`} variant="ghost" size="sm" icon={<Icon icon={IconEdit} size="sm" />} onClick={() => setEditing(g)} />
          <IconButton label={`New join token for ${g.name}`} variant="ghost" size="sm" icon={<Icon icon={IconKey} size="sm" />} onClick={() => setRotating(g)} />
          <IconButton label={`Delete ${g.name}`} variant="ghost" size="sm" icon={<Icon icon={IconTrash} size="sm" />} onClick={() => setDeleting(g)} />
        </HStack>
      ),
    },
  ];

  return (
    <Card padding={0}>
      <VStack gap={0}>
        <HStack padding={4} hAlign="between" vAlign="center">
          <Heading level={4}>Gateway groups</Heading>
        </HStack>
        <Divider />
        {groups.isLoading ? (
          <Loading />
        ) : groups.error ? (
          <ErrorState error={groups.error} onRetry={() => groups.refetch()} />
        ) : (groups.data?.length ?? 0) === 0 ? (
          <Empty
            title="No gateway groups"
            description="Create a group to get a join token, then start gateways with it wherever your traffic is."
            action={<Button label="New group" variant="primary" icon={<Icon icon={IconPlus} size="sm" />} onClick={onNew} />}
          />
        ) : (
          <Table
            data={(groups.data ?? []) as Array<GatewayGroup & Record<string, unknown>>}
            columns={columns}
            idKey="id"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </VStack>

      {editing && (
        <FormDialog isOpen onClose={() => setEditing(null)} title={`Edit ${editing.name}`}>
          <GroupForm group={editing} onClose={() => setEditing(null)} onToken={setToken} />
        </FormDialog>
      )}
      {token && <JoinTokenDialog token={token} onClose={() => setToken(null)} />}
      <Confirm
        target={rotating}
        title="New join token"
        description={`Replace the join token of "${rotating?.name}"? Running gateways keep working; new ones need the new token.`}
        actionLabel="Replace token"
        isLoading={rotate.isPending}
        onCancel={() => setRotating(null)}
        onConfirm={async () => {
          if (!rotating) return;
          try {
            const res = (await rotate.mutateAsync(rotating.id)) as { join_token: string };
            setToken(res.join_token);
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setRotating(null);
        }}
      />
      <Confirm
        target={deleting}
        title="Delete gateway group"
        description={`Delete "${deleting?.name}"? Its gateways stop getting config at once and stop serving within 15 minutes.`}
        actionLabel="Delete"
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Gateway group deleted" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setDeleting(null);
        }}
      />
    </Card>
  );
}

export default function Gateways() {
  const [showGroupForm, setShowGroupForm] = useState(false);
  const [joinToken, setJoinToken] = useState<string | null>(null);
  const newGroup = () => setShowGroupForm(true);

  return (
    <Page
      eyebrow="Operations"
      title="Gateways"
      description="Gateways serve your traffic close to your users and take their config from here. Each group has one join token; start as many gateways with it as you need."
      action={<Button label="New group" variant="primary" icon={<Icon icon={IconPlus} size="sm" />} onClick={newGroup} />}
    >
      <VStack gap={4}>
        <ClusterTable />
        <GatewayGroups onNew={newGroup} />
        <ClusterEvents />
      </VStack>

      {showGroupForm && (
        <FormDialog isOpen onClose={() => setShowGroupForm(false)} title="New gateway group">
          <GroupForm onClose={() => setShowGroupForm(false)} onToken={setJoinToken} />
        </FormDialog>
      )}
      {joinToken && <JoinTokenDialog token={joinToken} onClose={() => setJoinToken(null)} />}
    </Page>
  );
}
