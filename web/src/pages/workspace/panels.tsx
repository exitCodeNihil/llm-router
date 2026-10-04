import { useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Code } from "@astryxdesign/core/Code";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { Divider } from "@astryxdesign/core/Divider";
import { Icon } from "@astryxdesign/core/Icon";
import { IconButton } from "@astryxdesign/core/IconButton";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { WorkspaceTemplate } from "../../lib/types";
import {
  useCreateWorkspaceTemplate,
  useDeleteWorkspaceTemplate,
  useDeleteWorkspaceUserFile,
  useUpdateWorkspaceTemplate,
  useUploadWorkspaceUserFiles,
  useWorkspaceTemplates,
  useWorkspaceUserFiles,
} from "../../lib/hooks";
import { postNDJSON } from "../../lib/stream";
import { Confirm, FormActions, FormDialog } from "../../components/dialogs";
import { IconEdit, IconPlus, IconTrash } from "../../components/icons";
import { Empty, ErrorState, Loading } from "../../components/Page";

// ── Your files: injected into every workspace the user owns ──────

export function FilesCard() {
  const files = useWorkspaceUserFiles();
  const upload = useUploadWorkspaceUserFiles();
  const del = useDeleteWorkspaceUserFile();
  const [removingFile, setRemovingFile] = useState<string | null>(null);
  const showToast = useToast();
  const [folder, setFolder] = useState(".ssh");
  const input = useRef<HTMLInputElement>(null);

  async function onPick(list: FileList | null) {
    if (!list?.length) return;
    const form = new FormData();
    form.set("path", folder.trim());
    for (const f of Array.from(list)) form.append("file", f, f.name);
    try {
      await upload.mutateAsync(form);
      showToast({
        body: `Stored ${list.length} file${list.length === 1 ? "" : "s"}`,
      });
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Upload failed",
        type: "error",
      });
    } finally {
      if (input.current) input.current.value = "";
    }
  }

  return (
    <Card>
      <VStack gap={3} padding={3}>
        <VStack gap={0.5}>
          <Heading level={4}>Your files</Heading>
          <Text type="supporting" color="secondary">
            Written into <Code>$HOME</Code> of every workspace you own, on every
            start — ssh keys,
            <Code>.gitconfig</Code>, <Code>.npmrc</Code>, cloud credentials.
            Stored encrypted; anything under <Code>.ssh/</Code> is written 0600.
          </Text>
        </VStack>
        <HStack gap={2} vAlign="end" wrap="wrap">
          <TextInput
            label="Folder in $HOME"
            size="sm"
            value={folder}
            onChange={setFolder}
            placeholder=".ssh"
            description="Blank for $HOME itself"
            width={200}
          />
          <input
            ref={input}
            type="file"
            multiple
            hidden
            onChange={(e) => onPick(e.target.files)}
            aria-label="Choose files to keep"
          />
          <Button
            label="Add files"
            variant="secondary"
            size="sm"
            icon={<Icon icon={IconPlus} size="sm" />}
            isLoading={upload.isPending}
            onClick={() => input.current?.click()}
          />
        </HStack>
        {!!files.data?.length && (
          <>
            <Divider />
            <VStack gap={1}>
              {files.data.map((f) => (
                <HStack key={f.path} gap={2} vAlign="center" hAlign="between">
                  <HStack gap={2} vAlign="center">
                    <Code>~/{f.path}</Code>
                    <Text type="supporting" color="secondary">
                      {f.size} B · {f.mode === 384 ? "0600" : "0644"} ·{" "}
                      <Timestamp value={f.updated_at} />
                    </Text>
                  </HStack>
                  <IconButton
                    label={`Remove ${f.path}`}
                    variant="ghost"
                    size="sm"
                    icon={<Icon icon={IconTrash} size="sm" />}
                    onClick={() => setRemovingFile(f.path)}
                  />
                </HStack>
              ))}
            </VStack>
          </>
        )}
      </VStack>
      <Confirm
        target={removingFile}
        title="Remove stored file"
        description={`Remove ~/${removingFile} from your stored files? It stays in workspaces that already have it until their next start.`}
        actionLabel="Remove"
        isLoading={del.isPending}
        onCancel={() => setRemovingFile(null)}
        onConfirm={async () => {
          try {
            await del.mutateAsync(removingFile!);
            showToast({ body: "File removed" });
          } catch (err) {
            showToast({
              body: err instanceof Error ? err.message : "Failed",
              type: "error",
            });
          }
          setRemovingFile(null);
        }}
      />
    </Card>
  );
}

// ── Templates: the admin-curated menu of images ───────────────────

function TemplateForm({
  editing,
  onClose,
}: {
  editing: WorkspaceTemplate | null;
  onClose: () => void;
}) {
  const create = useCreateWorkspaceTemplate();
  const update = useUpdateWorkspaceTemplate();
  const showToast = useToast();
  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [image, setImage] = useState(editing?.image ?? "");
  const [dockerfile, setDockerfile] = useState(editing?.dockerfile ?? "");
  const [log, setLog] = useState<string | null>(null);
  const [building, setBuilding] = useState(false);

  async function save() {
    const body = {
      name: name.trim(),
      description,
      image: image.trim(),
      dockerfile,
    };
    if (editing) await update.mutateAsync({ id: editing.id, ...body });
    else await create.mutateAsync(body);
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await save();
      showToast({ body: editing ? "Template saved" : "Template added" });
      onClose();
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  // Save first so the build uses what is on screen, then stream the daemon.
  async function build() {
    if (!editing) return;
    setBuilding(true);
    setLog("");
    try {
      await save();
      await postNDJSON(
        `/api/workspace-templates/${editing.id}/build`,
        {},
        (line) => {
          if (line.stream === "error")
            setLog((l) => `${l ?? ""}\nERROR: ${line.data}\n`);
          else if (line.stream === "exit")
            setLog((l) => `${l ?? ""}\n— built ${image.trim()} —\n`);
          else setLog((l) => (l ?? "") + line.data);
        },
      );
    } catch (err) {
      setLog(
        (l) =>
          `${l ?? ""}\nERROR: ${err instanceof Error ? err.message : "build failed"}\n`,
      );
    } finally {
      setBuilding(false);
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <HStack gap={3} wrap="wrap">
          <TextInput
            label="Name"
            value={name}
            onChange={setName}
            placeholder="go"
            isRequired
            width={200}
          />
          <TextInput
            label="Image tag"
            value={image}
            onChange={setImage}
            placeholder="llmr-workspace-go"
            description="What the runtime pulls or builds"
            isRequired
            width={280}
          />
        </HStack>
        <TextInput
          label="Description"
          value={description}
          onChange={setDescription}
          placeholder="What is in it, and what persists"
          isOptional
        />
        <TextArea
          label="Dockerfile"
          value={dockerfile}
          onChange={setDockerfile}
          rows={16}
          description="Keep HOME=/workspace/.home and WORKDIR /workspace: only /workspace is on the volume. Caches under $HOME persist."
          isOptional
        />
        {log !== null && (
          <CodeBlock
            code={log || "…"}
            language="plaintext"
            title="build output"
            maxHeight={260}
            width="100%"
          />
        )}
        <HStack gap={2} hAlign="between" vAlign="center">
          <div>
            {editing && (
              <Button
                label={building ? "Building…" : "Build image"}
                variant="secondary"
                isLoading={building}
                isDisabled={!dockerfile.trim() || !image.trim()}
                onClick={build}
              />
            )}
          </div>
          <FormActions
            onCancel={onClose}
            submitLabel={editing ? "Save" : "Add template"}
            isLoading={create.isPending || update.isPending}
            isDisabled={!name.trim() || !image.trim()}
          />
        </HStack>
      </VStack>
    </form>
  );
}

export function TemplatesCard() {
  const templates = useWorkspaceTemplates();
  const del = useDeleteWorkspaceTemplate();
  const showToast = useToast();
  const [editing, setEditing] = useState<WorkspaceTemplate | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [deleting, setDeleting] = useState<WorkspaceTemplate | null>(null);

  return (
    <Card>
      <VStack gap={3} padding={3}>
        <HStack gap={2} hAlign="between" vAlign="center">
          <VStack gap={0.5}>
            <Heading level={4}>Templates</Heading>
            <Text type="supporting" color="secondary">
              What users pick from when creating a workspace. Edit the
              Dockerfile and build it here (Docker / podman); on Kubernetes,
              build with your CI and push the tag.
            </Text>
          </VStack>
          <Button
            label="New template"
            variant="secondary"
            size="sm"
            icon={<Icon icon={IconPlus} size="sm" />}
            onClick={() => {
              setEditing(null);
              setShowForm(true);
            }}
          />
        </HStack>
        <Divider />
        {templates.isLoading ? (
          <Loading />
        ) : templates.error ? (
          <ErrorState
            error={templates.error}
            onRetry={() => templates.refetch()}
          />
        ) : (templates.data?.length ?? 0) === 0 ? (
          <Empty
            title="No templates yet"
            description="Add one, or restart the gateway to reseed the built-in set."
          />
        ) : null}
        <VStack gap={1}>
          {(templates.data ?? []).map((t) => (
            <HStack key={t.id} gap={2} vAlign="center" hAlign="between">
              <HStack gap={2} vAlign="center">
                <Text type="body" weight="medium">
                  {t.name}
                </Text>
                <Code>{t.image}</Code>
                <Badge
                  variant={t.source === "seed" ? "neutral" : "blue"}
                  label={t.source === "seed" ? "built in" : "custom"}
                />
                <Text type="supporting" color="secondary" maxLines={1}>
                  {t.description}
                </Text>
              </HStack>
              <HStack gap={1}>
                <IconButton
                  label={`Edit ${t.name}`}
                  variant="ghost"
                  size="sm"
                  icon={<Icon icon={IconEdit} size="sm" />}
                  onClick={() => {
                    setEditing(t);
                    setShowForm(true);
                  }}
                />
                <IconButton
                  label={`Delete ${t.name}`}
                  variant="ghost"
                  size="sm"
                  icon={<Icon icon={IconTrash} size="sm" />}
                  onClick={() => setDeleting(t)}
                />
              </HStack>
            </HStack>
          ))}
        </VStack>
      </VStack>

      {showForm && (
        <FormDialog
          isOpen
          onClose={() => setShowForm(false)}
          title={editing ? `Edit template · ${editing.name}` : "New template"}
          wide
        >
          <TemplateForm editing={editing} onClose={() => setShowForm(false)} />
        </FormDialog>
      )}
      <Confirm
        target={deleting}
        title="Delete template"
        description={`Remove "${deleting?.name}"? Existing workspaces keep their image; nobody can create new ones from it.${deleting?.source === "seed" ? " Built-in templates come back on the next gateway restart." : ""}`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Template deleted" });
          } catch (err) {
            showToast({
              body: err instanceof Error ? err.message : "Failed",
              type: "error",
            });
          }
          setDeleting(null);
        }}
      />
    </Card>
  );
}
