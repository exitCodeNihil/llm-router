import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { Grid } from "@astryxdesign/core/Grid";
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
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { EdgeNode, Node } from "../lib/types";
import { useCreateEdgeNode, useDeleteEdgeNode, useEdgeNodes, useNodes } from "../lib/hooks";
import { int } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog, SecretDialog } from "../components/dialogs";
import { IconPlus, IconTrash } from "../components/icons";

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
            </HStack>
            <Code>{n.addr}</Code>
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

// ── Registered edge nodes ───────────────────────────────────────
function RegisterForm({ onClose, onToken }: { onClose: () => void; onToken: (t: string) => void }) {
  const create = useCreateEdgeNode();
  const showToast = useToast();
  const [name, setName] = useState("");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      const res = (await create.mutateAsync({ name: name.trim() })) as { token: string };
      onToken(res.token);
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Node name"
          value={name}
          onChange={setName}
          placeholder="eu-west-gateway"
          description="Used in dashboards and cluster events"
          isRequired
          hasAutoFocus
        />
        <FormActions
          onCancel={onClose}
          submitLabel="Register node"
          isLoading={create.isPending}
          isDisabled={!name.trim()}
        />
      </VStack>
    </form>
  );
}

export default function EdgeNodes() {
  const edges = useEdgeNodes();
  const del = useDeleteEdgeNode();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [nodeToken, setNodeToken] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<EdgeNode | null>(null);

  const columns: TableColumn<EdgeNode & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Name",
      width: proportional(1.4),
      renderCell: (n) => (
        <Text type="body" weight="medium" maxLines={1}>
          {n.name}
        </Text>
      ),
    },
    {
      key: "last_seen_at",
      header: "Last seen",
      width: pixel(140),
      renderCell: (n) =>
        n.last_seen_at ? (
          <Timestamp value={n.last_seen_at} format="relative" isLive />
        ) : (
          <Badge variant="warning" label="never" />
        ),
    },
    {
      key: "last_seen_version",
      header: "Version",
      width: pixel(130),
      renderCell: (n) => (n.last_seen_version ? <Code>{n.last_seen_version}</Code> : "—"),
    },
    {
      key: "created_at",
      header: "Registered",
      width: pixel(120),
      renderCell: (n) => <Timestamp value={n.created_at} format="date" />,
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(64),
      align: "end",
      renderCell: (n) => (
        <IconButton
          label={`Revoke ${n.name}`}
          variant="ghost"
          size="sm"
          icon={<Icon icon={IconTrash} size="sm" />}
          onClick={() => setDeleting(n)}
        />
      ),
    },
  ];

  const addButton = (
    <Button
      label="Register node"
      variant="primary"
      icon={<Icon icon={IconPlus} size="sm" />}
      onClick={() => setShowForm(true)}
    />
  );

  return (
    <Page
      eyebrow="Operations"
      title="Edge nodes"
      description="Stateless gateways that sync a signed config snapshot, enforce limits locally, and ship usage back in batches."
      action={addButton}
    >
      <VStack gap={4}>
        <ClusterTable />

        <Grid columns={{ minWidth: 320, repeat: "fit" }} gap={4}>
          <Card padding={0}>
            <VStack gap={0}>
              <HStack padding={4}>
                <Heading level={4}>Registered nodes</Heading>
              </HStack>
              <Divider />
              {edges.isLoading ? (
                <Loading />
              ) : edges.error ? (
                <ErrorState error={edges.error} onRetry={() => edges.refetch()} />
              ) : (edges.data?.length ?? 0) === 0 ? (
                <Empty
                  title="No edge nodes"
                  description="Register a node to get its bootstrap token."
                  action={addButton}
                />
              ) : (
                <Table
                  data={(edges.data ?? []) as Array<EdgeNode & Record<string, unknown>>}
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

          <ClusterEvents />
        </Grid>
      </VStack>

      {showForm && (
        <FormDialog isOpen onClose={() => setShowForm(false)} title="Register edge node">
          <RegisterForm onClose={() => setShowForm(false)} onToken={setNodeToken} />
        </FormDialog>
      )}

      {nodeToken && (
        <SecretDialog secret={nodeToken} onClose={() => setNodeToken(null)} title="Edge node registered" noun="node token" />
      )}

      <Confirm
        target={deleting}
        title="Revoke edge node"
        description={`Revoke "${deleting?.name}"? It will stop receiving config snapshots on its next sync.`}
        actionLabel="Revoke"
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Edge node revoked" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setDeleting(null);
        }}
      />
    </Page>
  );
}
