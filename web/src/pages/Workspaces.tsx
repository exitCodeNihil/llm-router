import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Code } from "@astryxdesign/core/Code";
import { Grid } from "@astryxdesign/core/Grid";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Icon } from "@astryxdesign/core/Icon";
import { IconButton } from "@astryxdesign/core/IconButton";
import { StatusDot } from "@astryxdesign/core/StatusDot";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { Me, Workspace } from "../lib/types";
import {
  useCreateWorkspace,
  useDeleteWorkspace,
  useMe,
  useStartWorkspace,
  useStopWorkspace,
  useWorkspaceSettings,
  useWorkspaceTemplates,
  useWorkspaces,
} from "../lib/hooks";
import { Selector } from "@astryxdesign/core/Selector";
import { FilesCard } from "./workspace/panels";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import { IconPlus, IconTrash } from "../components/icons";

function CreateForm({ onClose, isAdmin }: { onClose: () => void; isAdmin: boolean }) {
  const create = useCreateWorkspace();
  const settings = useWorkspaceSettings();
  const templates = useWorkspaceTemplates();
  const showToast = useToast();
  const [name, setName] = useState("");
  const [templateId, setTemplateId] = useState("");
  const [image, setImage] = useState("");

  const fallback = settings.data?.default_image ?? "";
  const list = templates.data ?? [];
  const chosen = list.find((t) => t.id === (templateId || list[0]?.id));
  // Admins may type an image that is not on the menu.
  const custom = isAdmin && templateId === "custom";

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await create.mutateAsync(
        custom
          ? { name: name.trim(), image: image.trim() || fallback }
          : { name: name.trim(), template_id: chosen?.id },
      );
      showToast({ body: "Workspace created" });
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          placeholder="scratch"
          description="Just a label — the workspace is yours alone"
          isRequired
        />
        <Selector
          label="Template"
          value={templateId || list[0]?.id || ""}
          onChange={setTemplateId}
          description={custom ? undefined : chosen?.description}
          options={[
            ...list.map((t) => ({ value: t.id, label: `${t.name} — ${t.image}` })),
            ...(isAdmin ? [{ value: "custom", label: "Custom image…" }] : []),
          ]}
        />
        {custom && (
          <TextInput
            label="Image"
            value={image}
            onChange={setImage}
            placeholder={fallback || "llmr-workspace-base"}
            description="Any image with git, pi and code-server on PATH, starting code-server on 8080"
          />
        )}
        <FormActions
          onCancel={onClose}
          submitLabel="Create workspace"
          isLoading={create.isPending}
          isDisabled={!name.trim() || (custom ? !image.trim() && !fallback : !chosen)}
        />
      </VStack>
    </form>
  );
}

function WorkspaceCard({ ws, me }: { ws: Workspace; me: Me }) {
  const navigate = useNavigate();
  // Admins see everyone's workspaces for containment only: stop and delete,
  // never a shell in someone else's container.
  const owner = ws.user_id === me.user_id;
  const start = useStartWorkspace();
  const stop = useStopWorkspace();
  const del = useDeleteWorkspace();
  const showToast = useToast();
  const [confirming, setConfirming] = useState<Workspace | null>(null);

  const running = ws.status === "running";
  const busy = start.isPending || stop.isPending;

  async function toggle() {
    try {
      if (running) {
        await stop.mutateAsync(ws.id);
      } else {
        await start.mutateAsync(ws.id);
      }
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <>
      <Card>
        <VStack gap={3} padding={3}>
          <HStack gap={2} hAlign="between" vAlign="start">
            <HStack gap={2} vAlign="center">
              <StatusDot
                variant={running ? "success" : ws.status === "error" ? "error" : "neutral"}
                label={ws.status}
              />
              <Text type="body" weight="medium" maxLines={1}>
                {ws.name}
              </Text>
            </HStack>
            {ws.status === "error" && <Badge variant="error" label="error" />}
          </HStack>

          <VStack gap={1}>
            <Code>{ws.image}</Code>
            <Text type="supporting" color="secondary">
              {!owner && ws.owner_email ? `${ws.owner_email} · ` : ""}created <Timestamp value={ws.created_at} />
            </Text>
          </VStack>

          <HStack gap={2} hAlign="between" vAlign="center">
            <HStack gap={2}>
              <Button
                label={running ? "Stop" : "Start"}
                variant="secondary"
                size="sm"
                isLoading={busy}
                onClick={toggle}
              />
              {owner && (
                <Button
                  label="Open"
                  variant="primary"
                  size="sm"
                  // The IDE needs a live container to list files or exec.
                  isDisabled={!running}
                  onClick={() => navigate(`/workspaces/${ws.id}`)}
                />
              )}
            </HStack>
            <IconButton
              label={`Delete ${ws.name}${!owner && ws.owner_email ? ` (${ws.owner_email})` : ""}`}
              variant="ghost"
              size="sm"
              icon={<Icon icon={IconTrash} size="sm" />}
              onClick={() => setConfirming(ws)}
            />
          </HStack>
        </VStack>
      </Card>

      <Confirm
        target={confirming}
        title={`Delete ${ws.name}?`}
        description="The container and its volume go with it — code, uploaded keys and chat history are all deleted. This cannot be undone."
        isLoading={del.isPending}
        onCancel={() => setConfirming(null)}
        onConfirm={async () => {
          try {
            await del.mutateAsync(ws.id);
            showToast({ body: "Workspace deleted" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setConfirming(null);
        }}
      />
    </>
  );
}

export default function Workspaces() {
  const me = useMe();
  const isAdmin = !!me.data?.is_admin;
  const allowed = !!me.data?.workspaces;
  const workspaces = useWorkspaces(allowed);
  const navigate = useNavigate();
  const [creating, setCreating] = useState(false);

  if (!allowed) {
    return (
      <Page eyebrow="Develop" title="Workspaces" description="A container you own, with VS Code and a terminal.">
        <Empty
          title={isAdmin ? "Workspaces are switched off" : "Workspaces aren't enabled for your account"}
          description={
            isAdmin
              ? "Turn them on and choose who may use them in Workspace settings."
              : "Ask an admin to enable workspaces and add you or one of your teams."
          }
          action={
            isAdmin ? (
              <Button label="Open Workspace settings" variant="primary" onClick={() => navigate("/workspaces/settings")} />
            ) : undefined
          }
        />
      </Page>
    );
  }

  return (
    <Page
      eyebrow="Develop"
      title="Workspaces"
      description="A container you own, with VS Code and a terminal; run pi from the terminal for an agent."
      action={
        <Button
          label="New workspace"
          variant="primary"
          icon={<Icon icon={IconPlus} size="sm" />}
          onClick={() => setCreating(true)}
        />
      }
    >
      {workspaces.isLoading ? (
        <Loading />
      ) : workspaces.error ? (
        <ErrorState error={workspaces.error} onRetry={() => workspaces.refetch()} />
      ) : !workspaces.data?.length ? (
        <Empty
          title="No workspaces yet"
          description="Create one to get VS Code and a terminal in a container of your own."
        />
      ) : (
        <Grid columns={{ minWidth: 300, max: 3 }} gap={3}>
          {workspaces.data.map((ws) => (
            <WorkspaceCard key={ws.id} ws={ws} me={me.data!} />
          ))}
        </Grid>
      )}

      {me.data?.user_id ? (
        <VStack maxWidth={760}>
          <FilesCard />
        </VStack>
      ) : null}

      <FormDialog
        isOpen={creating}
        onClose={() => setCreating(false)}
        title="New workspace"
        subtitle="A container with VS Code, git and a shell; pi is on the terminal"
      >
        <CreateForm onClose={() => setCreating(false)} isAdmin={isAdmin} />
      </FormDialog>
    </Page>
  );
}
