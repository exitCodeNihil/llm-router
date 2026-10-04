import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import { Selector } from "@astryxdesign/core/Selector";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { Provider, ProviderType } from "../lib/types";
import { useCreateProvider, useDeleteProvider, useProviders, useUpdateProvider } from "../lib/hooks";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import { IconEdit, IconPlus, IconTrash } from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";

const AUTH_MODES: Record<ProviderType, { value: string; label: string; needsKey: boolean }[]> = {
  azure: [
    { value: "entra", label: "Entra ID (managed identity)", needsKey: false },
    { value: "api_key", label: "API key", needsKey: true },
  ],
  openai_compatible: [
    { value: "bearer", label: "Bearer token", needsKey: true },
    { value: "oauth_passthrough", label: "Forward the caller's token", needsKey: false },
    { value: "none", label: "No auth", needsKey: false },
  ],
  gcp_vertex: [
    { value: "gcp_adc", label: "Application Default Credentials", needsKey: false },
    { value: "gcp_sa", label: "Service account key (JSON)", needsKey: true },
  ],
  openrouter: [{ value: "bearer", label: "API key", needsKey: true }],
};

const OPENROUTER_URL = "https://openrouter.ai/api/v1";

const TYPE_LABELS: Record<ProviderType, string> = {
  azure: "Azure",
  openai_compatible: "OpenAI-compatible",
  gcp_vertex: "Google Vertex AI",
  openrouter: "OpenRouter",
};

// Vertex's host follows the region; "global" is the one exception.
function vertexHost(location: string) {
  const loc = location.trim() || "global";
  return loc === "global" ? "https://aiplatform.googleapis.com" : `https://${loc}-aiplatform.googleapis.com`;
}

function ProviderForm({ editing, onClose }: { editing: Provider | null; onClose: () => void }) {
  const create = useCreateProvider();
  const update = useUpdateProvider();
  const showToast = useToast();

  const [name, setName] = useState(editing?.name ?? "");
  const [type, setType] = useState<ProviderType>(editing?.type ?? "azure");
  const [baseUrl, setBaseUrl] = useState(editing?.base_url ?? "");
  const [authMode, setAuthMode] = useState(editing?.auth_mode ?? "entra");
  const [apiKey, setApiKey] = useState("");
  const [apiVersion, setApiVersion] = useState((editing?.config?.api_version as string) ?? "");
  const [project, setProject] = useState((editing?.config?.project as string) ?? "");
  const [location, setLocation] = useState((editing?.config?.location as string) ?? "global");

  const modes = AUTH_MODES[type];
  const needsKey = modes.find((m) => m.value === authMode)?.needsKey ?? false;
  // A stored secret only "keeps" for the mode it was entered under; changing
  // the mode clears it server-side, so the field is required again.
  const hasKey = !!editing?.has_api_key && editing.auth_mode === authMode;
  const busy = create.isPending || update.isPending;

  function onTypeChange(t: ProviderType) {
    setType(t);
    setAuthMode(AUTH_MODES[t][0].value); // reset to a mode valid for this type
    // One address for everyone; shown so a proxy can still be substituted —
    // and taken away again if the type changes, so it never lands on Azure.
    if (t === "openrouter" && !baseUrl.trim()) setBaseUrl(OPENROUTER_URL);
    if (t !== "openrouter" && baseUrl.trim() === OPENROUTER_URL) setBaseUrl("");
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    // Always sent for Azure so that clearing the field clears the stored value.
    const config =
      type === "azure"
        ? { api_version: apiVersion.trim() }
        : type === "gcp_vertex"
          ? { project: project.trim(), location: location.trim() || "global" }
          : undefined;
    // Vertex derives its host from the location server-side; only an explicit
    // private endpoint is stored, so a later region change cannot go stale.
    const url = baseUrl.trim();
    try {
      if (editing) {
        await update.mutateAsync({
          id: editing.id,
          name: name.trim(),
          base_url: url,
          auth_mode: authMode,
          ...(apiKey ? { api_key: apiKey } : {}),
          ...(config ? { config } : {}),
        });
        showToast({ body: "Provider updated" });
      } else {
        await create.mutateAsync({
          name: name.trim(),
          type,
          base_url: url,
          auth_mode: authMode,
          ...(needsKey && apiKey ? { api_key: apiKey } : {}),
          ...(config ? { config } : {}),
        });
        showToast({ body: "Provider added" });
      }
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  const urlHint =
    type === "azure"
      ? "https://<resource>.openai.azure.com or an AI Foundry endpoint"
      : type === "gcp_vertex"
        ? `Leave blank to use ${vertexHost(location)}; set only for a private endpoint`
        : type === "openrouter"
          ? "OpenRouter's API; change only to route through a proxy"
          : "Include the /v1 suffix, e.g. http://vllm:8000/v1";
  const isVertex = type === "gcp_vertex";

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          placeholder={
            type === "azure" ? "e.g. azure-eastus" : type === "gcp_vertex" ? "e.g. vertex-prod" : type === "openrouter" ? "e.g. openrouter" : "e.g. vllm-lab"
          }
          isRequired
          hasAutoFocus
        />

        <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
          <Selector
            label="Type"
            value={type}
            onChange={(v) => onTypeChange(v as ProviderType)}
            isDisabled={!!editing}
            disabledMessage="Provider type cannot change after creation"
            options={[
              { value: "azure", label: "Azure OpenAI / AI Foundry" },
              { value: "gcp_vertex", label: "Google Vertex AI" },
              { value: "openrouter", label: "OpenRouter" },
              { value: "openai_compatible", label: "OpenAI-compatible" },
            ]}
          />
          <Selector
            label="Auth mode"
            value={authMode}
            onChange={setAuthMode}
            description={
              authMode === "oauth_passthrough"
                ? "The caller's Authorization header travels upstream. Clients send their gateway key as X-Llmr-Key instead."
                : authMode === "gcp_adc"
                  ? "The attached service account, workload identity, or `gcloud auth application-default login` on the gateway host."
                    : undefined
            }
            options={modes.map((m) => ({ value: m.value, label: m.label }))}
          />
        </Grid>

        {isVertex && (
          <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
            <TextInput
              label="Project ID"
              value={project}
              onChange={setProject}
              placeholder="my-gcp-project"
              isRequired
            />
            <TextInput
              label="Location"
              value={location}
              onChange={setLocation}
              placeholder="global"
              description="global serves Gemini and the MaaS models; Claude needs a region such as us-east5"
            />
          </Grid>
        )}

        <TextInput
          label="Base URL"
          value={baseUrl}
          onChange={setBaseUrl}
          placeholder={
            type === "azure"
              ? "https://my-resource.openai.azure.com"
              : isVertex
                ? vertexHost(location)
                : "http://vllm:8000/v1"
          }
          description={urlHint}
          isRequired={!isVertex}
          isOptional={isVertex}
        />

        {needsKey && authMode === "gcp_sa" ? (
          <TextArea
            label={editing ? "Replace service account key" : "Service account key"}
            value={apiKey}
            onChange={setApiKey}
            placeholder={
              hasKey
                ? "•••••••• (leave blank to keep)"
                : '{ "type": "service_account", "project_id": "…", "private_key": "…" }'
            }
            description="Paste the downloaded JSON key. Stored encrypted at rest with LLMR_ENCRYPTION_KEY; the gateway signs its own tokens with it."
            rows={5}
            isRequired={!hasKey}
          />
        ) : needsKey ? (
          <TextInput
            label={editing ? "Replace API key" : "API key"}
            type="password"
            value={apiKey}
            onChange={setApiKey}
            placeholder={hasKey ? "•••••••• (leave blank to keep)" : ""}
            description="Stored encrypted at rest with LLMR_ENCRYPTION_KEY"
            isRequired={!hasKey}
          />
        ) : null}

        {type === "azure" && (
          <TextInput
            label="API version"
            value={apiVersion}
            onChange={setApiVersion}
            placeholder="2024-10-21"
            isOptional
            description="Defaults to 2024-10-21 when blank"
          />
        )}

        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Add provider"}
          isLoading={busy}
          isDisabled={
            !name.trim() ||
            (isVertex ? !project.trim() : type !== "openrouter" && !baseUrl.trim()) ||
            (needsKey && !hasKey && !apiKey)
          }
        />
      </VStack>
    </form>
  );
}

export default function Providers() {
  const providers = useProviders();
  const del = useDeleteProvider();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [deleting, setDeleting] = useState<Provider | null>(null);

  const columns: TableColumn<Provider & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Name",
      sortable: true,
      width: proportional(1),
      renderCell: (p) => (
        <Text type="body" weight="medium" maxLines={1}>
          {p.name}
        </Text>
      ),
    },
    {
      key: "type",
      header: "Type",
      sortable: true,
      width: pixel(176),
      renderCell: (p) => (
        <Badge
          variant={
            p.type === "azure"
              ? "blue"
              : p.type === "gcp_vertex"
                ? "green"
                : p.type === "openrouter"
                  ? "purple"
                  : "neutral"
          }
          label={TYPE_LABELS[p.type]}
        />
      ),
    },
    {
      key: "base_url",
      header: "Base URL",
      width: proportional(1.6),
      renderCell: (p) => (
        <Code>
          {p.type === "gcp_vertex" && p.config?.project
            ? `${String(p.config.project)} · ${String(p.config.location ?? "global")}`
            : p.base_url || (p.type === "openrouter" ? OPENROUTER_URL : "")}
        </Code>
      ),
    },
    {
      key: "auth_mode",
      header: "Auth",
      sortable: true,
      width: pixel(150),
      renderCell: (p) => (
        <HStack gap={1.5} vAlign="center">
          <Text type="supporting">{p.auth_mode}</Text>
          {p.has_api_key && <Badge variant="success" label="key set" />}
        </HStack>
      ),
    },
    {
      key: "created_at",
      header: "Added",
      sortable: true,
      width: pixel(110),
      renderCell: (p) => <Timestamp value={p.created_at} format="date" />,
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(88),
      align: "end",
      renderCell: (p) => (
        <HStack gap={1} hAlign="end">
          <IconButton
            label={`Edit ${p.name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconEdit} size="sm" />}
            onClick={() => {
              setEditing(p);
              setShowForm(true);
            }}
          />
          <IconButton
            label={`Delete ${p.name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={() => setDeleting(p)}
          />
        </HStack>
      ),
    },
  ];

  const tools = useTableTools(providers.data as Array<Provider & Record<string, unknown>> | undefined, {
    search: (p) => `${p.name} ${TYPE_LABELS[p.type]} ${p.base_url} ${p.auth_mode} ${String(p.config?.project ?? "")}`,
    defaultSort: [{ sortKey: "name", direction: "ascending" }],
  });

  const addButton = (
    <Button
      label="Add provider"
      variant="primary"
      icon={<Icon icon={IconPlus} size="sm" />}
      onClick={() => {
        setEditing(null);
        setShowForm(true);
      }}
    />
  );

  return (
    <Page
      eyebrow="Routing"
      title="Providers"
      description="Upstream backends: Azure OpenAI / AI Foundry, Google Vertex AI, OpenRouter, or any OpenAI-compatible server."
      action={
        <HStack gap={2} vAlign="center">
          <SearchBox value={tools.query} onChange={tools.setQuery} placeholder="Search providers…" />
          {addButton}
        </HStack>
      }
    >
      <Card padding={0}>
        {providers.isLoading ? (
          <Loading />
        ) : providers.error ? (
          <ErrorState error={providers.error} onRetry={() => providers.refetch()} />
        ) : (providers.data?.length ?? 0) === 0 ? (
          <Empty
            title="No providers yet"
            description="Register an upstream backend, then expose its models on the Models page."
            action={addButton}
          />
        ) : (
          <Table
            data={tools.rows}
            plugins={tools.plugins}
            columns={columns}
            idKey="id"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </Card>

      {showForm && (
        <FormDialog
          isOpen
          onClose={() => setShowForm(false)}
          title={editing ? "Edit provider" : "Add provider"}
          wide
        >
          <ProviderForm editing={editing} onClose={() => setShowForm(false)} />
        </FormDialog>
      )}

      <Confirm
        target={deleting}
        title="Delete provider"
        description={`Delete "${deleting?.name}"? Every model backed by it will also be removed.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Provider deleted" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setDeleting(null);
        }}
      />
    </Page>
  );
}
