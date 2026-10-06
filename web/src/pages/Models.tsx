import { useMemo, useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Code } from "@astryxdesign/core/Code";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { Switch } from "@astryxdesign/core/Switch";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { NumberInput } from "@astryxdesign/core/NumberInput";
import { Selector } from "@astryxdesign/core/Selector";
import { Typeahead } from "@astryxdesign/core/Typeahead";
import type { SearchableItem } from "@astryxdesign/core/Typeahead";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@astryxdesign/core/SegmentedControl";
import { Section } from "@astryxdesign/core/Section";
import { useToast } from "@astryxdesign/core/Toast";

import type { Deployment, Price, ProviderType } from "../lib/types";
import {
  useCreateDeployment,
  useDeleteDeployment,
  useDeployments,
  usePrices,
  useProviderModels,
  useProviders,
  useUpdateDeployment,
  useRoutingHealth,
} from "../lib/hooks";
import { money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import { IconEdit, IconPlus, IconTrash } from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";

type PricingMode = "catalog" | "custom";

// Providers whose discovery reports the underlying model, keyed to the price catalog.
const CATALOG_PREFIX: Partial<Record<ProviderType, string>> = {
  azure: "azure",
  gcp_vertex: "gcp",
  openrouter: "openrouter",
};

function DeploymentForm({
  editing,
  onClose,
}: {
  editing: Deployment | null;
  onClose: () => void;
}) {
  const create = useCreateDeployment();
  const update = useUpdateDeployment();
  const providers = useProviders();
  const showToast = useToast();

  const [providerId, setProviderId] = useState(editing?.provider_id ?? "");
  const [modelName, setModelName] = useState(editing?.model_name ?? "");
  const [upstreamName, setUpstreamName] = useState(
    editing?.upstream_name ?? "",
  );
  const [priority, setPriority] = useState<number>(editing?.priority ?? 0);
  const [pricingMode, setPricingMode] = useState<PricingMode>(
    editing && (editing.input_per_1m != null || editing.output_per_1m != null)
      ? "custom"
      : "catalog",
  );
  const [catalogId, setCatalogId] = useState(editing?.catalog_model_id ?? "");
  const [inputPrice, setInputPrice] = useState<number | null>(
    editing?.input_per_1m ?? null,
  );
  const [outputPrice, setOutputPrice] = useState<number | null>(
    editing?.output_per_1m ?? null,
  );
  const [cachedPrice, setCachedPrice] = useState<number | null>(
    editing?.cached_input_per_1m ?? null,
  );

  // Live discovery: ask the provider what it actually serves.
  const discovered = useProviderModels(providerId);
  const selectedProvider = (providers.data ?? []).find(
    (p) => p.id === providerId,
  );

  type UpstreamMeta = { model?: string; isManual?: boolean };

  const items: SearchableItem<UpstreamMeta>[] = useMemo(
    () =>
      (discovered.data ?? []).map((d) => ({
        id: d.id,
        label: d.id,
        auxiliaryData: { model: d.model },
      })),
    [discovered.data],
  );

  /**
   * Discovery is best-effort: the provider may be unreachable, may not expose a
   * models endpoint, or may simply not list a deployment yet. Always surface the
   * exact text typed as a selectable option so a name can be entered by hand —
   * otherwise those deployments could not be created at all.
   */
  const searchSource = useMemo(() => {
    const withManualEntry = (
      q: string,
      matches: SearchableItem<UpstreamMeta>[],
    ) => {
      const typed = q.trim();
      if (!typed || matches.some((i) => i.id === typed)) return matches;
      return [
        { id: typed, label: typed, auxiliaryData: { isManual: true } },
        ...matches,
      ];
    };
    return {
      search: (q: string) =>
        withManualEntry(
          q,
          items.filter((i) => i.label.toLowerCase().includes(q.toLowerCase())),
        ).slice(0, 50),
      bootstrap: () => items.slice(0, 20),
    };
  }, [items]);

  const selectedItem = upstreamName
    ? (items.find((i) => i.id === upstreamName) ?? {
        id: upstreamName,
        label: upstreamName,
      })
    : null;

  function pickUpstream(item: SearchableItem<UpstreamMeta> | null) {
    const value = item?.id ?? "";
    setUpstreamName(value);
    if (!value) return;
    const model = item?.auxiliaryData?.model;
    if (!modelName) setModelName(model ?? value);
    // Azure and Vertex report the underlying model — prefill catalog pricing.
    const prefix = selectedProvider && CATALOG_PREFIX[selectedProvider.type];
    if (model && prefix && pricingMode === "catalog" && !catalogId) {
      setCatalogId(`${prefix}/${model}`);
    }
  }

  const busy = create.isPending || update.isPending;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const pricing =
      pricingMode === "catalog"
        ? {
            catalog_model_id: catalogId.trim() || null,
            input_per_1m: null,
            output_per_1m: null,
            cached_input_per_1m: null,
          }
        : {
            catalog_model_id: null,
            input_per_1m: inputPrice,
            output_per_1m: outputPrice,
            // Left blank means "this upstream does not discount cache reads",
            // which is not the same as free — keep it null rather than 0.
            cached_input_per_1m: cachedPrice,
          };
    const base = {
      provider_id: providerId,
      model_name: modelName.trim(),
      upstream_name: upstreamName.trim(),
      priority,
      ...pricing,
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...base });
        showToast({ body: "Model updated" });
      } else {
        await create.mutateAsync(base);
        showToast({ body: "Model added" });
      }
      onClose();
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  const upstreamHint = !providerId
    ? "Pick a provider to discover its models"
    : discovered.isLoading
      ? "Asking the provider what it serves…"
      : discovered.isError
        ? "Discovery unavailable — type the name manually"
        : `${items.length} upstream${items.length === 1 ? "" : "s"} available`;

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <Selector
          label="Provider"
          value={providerId}
          onChange={setProviderId}
          placeholder="Select a provider…"
          isRequired
          options={(providers.data ?? []).map((p) => ({
            value: p.id,
            label: p.name,
          }))}
        />

        {selectedProvider?.auth_mode === "oauth_passthrough" && (
          <Banner
            status="warning"
            title="Not available in the Playground or workspaces"
            description="This provider forwards the caller's own login, so the model only works from clients that send one, such as Claude Code."
          />
        )}

        <Grid columns={{ minWidth: 220, repeat: "fit" }} gap={3}>
          <TextInput
            label="Model name"
            value={modelName}
            onChange={setModelName}
            placeholder="gpt-4o"
            description="What clients send as the model. End with * to match a prefix, e.g. claude-*"
            isRequired
          />
          <Typeahead
            label="Upstream model"
            value={selectedItem}
            onChange={pickUpstream}
            searchSource={searchSource}
            placeholder="gpt-4o-2024-08-06"
            description={upstreamHint}
            hasEntriesOnFocus
            isRequired
            renderItem={(item) => (
              <VStack gap={0}>
                <Text type="body">{item.label}</Text>
                {item.auxiliaryData?.isManual ? (
                  <Text type="supporting" color="secondary">
                    Use this name as typed
                  </Text>
                ) : (
                  item.auxiliaryData?.model &&
                  item.auxiliaryData.model !== item.id && (
                    <Text type="supporting" color="secondary">
                      {item.auxiliaryData.model}
                    </Text>
                  )
                )}
              </VStack>
            )}
          />
        </Grid>

        <NumberInput
          label="Priority"
          value={priority}
          onChange={setPriority}
          description="Lower routes first among backends sharing a model name"
          min={0}
        />

        <Section variant="muted" padding={3}>
          <VStack gap={3}>
            <SegmentedControl
              label="Pricing mode"
              size="sm"
              value={pricingMode}
              onChange={(v) => setPricingMode(v as PricingMode)}
            >
              <SegmentedControlItem value="catalog" label="Catalog" />
              <SegmentedControlItem value="custom" label="Custom" />
            </SegmentedControl>

            {pricingMode === "catalog" ? (
              <TextInput
                label="Catalog model id"
                value={catalogId}
                onChange={setCatalogId}
                placeholder="azure/gpt-4o-2024-08-06"
                description="Uses the native price catalog"
              />
            ) : (
              <Grid columns={{ minWidth: 160, repeat: "fit" }} gap={3}>
                <NumberInput
                  label="Input $/1M"
                  value={inputPrice}
                  onChange={setInputPrice}
                  min={0}
                  step={0.01}
                  placeholder="0.00"
                />
                <NumberInput
                  label="Output $/1M"
                  value={outputPrice}
                  onChange={setOutputPrice}
                  min={0}
                  step={0.01}
                  placeholder="0.00"
                />
                <NumberInput
                  label="Cached input $/1M"
                  value={cachedPrice}
                  onChange={setCachedPrice}
                  min={0}
                  step={0.01}
                  placeholder="0.00"
                  description="Blank bills cache reads at the input rate"
                />
              </Grid>
            )}
          </VStack>
        </Section>

        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Add model"}
          isLoading={busy}
          isDisabled={!providerId || !modelName.trim() || !upstreamName.trim()}
        />
      </VStack>
    </form>
  );
}

// The rates a backend will actually bill: a custom override, else the catalog.
function ratesOf(d: Deployment, prices: Map<string, Price>) {
  // Both rates, like the gateway: a half-set pair falls through to the catalog.
  const custom = d.input_per_1m != null && d.output_per_1m != null;
  const cat =
    !custom && d.catalog_model_id ? prices.get(d.catalog_model_id) : undefined;
  const input = custom ? (d.input_per_1m ?? 0) : cat?.input_per_1m;
  const output = custom ? (d.output_per_1m ?? 0) : cat?.output_per_1m;
  return { custom, input, output };
}

// Same rule as the gateway: priority, then cheapest, unpriced last, then name.
// The table shows rows in the order traffic will try them.
function routingOrder(
  a: Deployment,
  b: Deployment,
  prices: Map<string, Price>,
): number {
  if (a.model_name !== b.model_name)
    return a.model_name.localeCompare(b.model_name);
  if (a.priority !== b.priority) return a.priority - b.priority;
  const ra = ratesOf(a, prices);
  const rb = ratesOf(b, prices);
  const ka =
    ra.input != null && ra.output != null ? ra.input + ra.output : Infinity;
  const kb =
    rb.input != null && rb.output != null ? rb.input + rb.output : Infinity;
  if (ka !== kb) return ka - kb;
  return a.upstream_name.localeCompare(b.upstream_name);
}

// One shape for every row: the rates that will actually be billed, whether
// they come from a custom override or the catalog — a catalog id on its own
// told the reader nothing about the price.
function PricingCell({
  d,
  prices,
}: {
  d: Deployment;
  prices: Map<string, Price>;
}) {
  const { custom, input, output } = ratesOf(d, prices);
  if (input == null || output == null) {
    return (
      <Text type="supporting" color="disabled">
        {d.catalog_model_id
          ? `${d.catalog_model_id} (not in catalog)`
          : "unpriced"}
      </Text>
    );
  }
  return (
    <span className="tnum">
      <Text type="supporting" color="secondary">
        {money(input)} / {money(output)}
        {!custom && " · catalog"}
      </Text>
    </span>
  );
}

export default function Models() {
  const deployments = useDeployments();
  const priceRows = usePrices();
  const prices = useMemo(
    () => new Map((priceRows.data ?? []).map((p) => [p.model_id, p])),
    [priceRows.data],
  );
  const update = useUpdateDeployment();
  const del = useDeleteDeployment();
  const showToast = useToast();
  const health = useRoutingHealth();
  const cooling = useMemo(
    () => new Map((health.data ?? []).map((c) => [c.deployment_id, c])),
    [health.data],
  );
  const providerRows = useProviders();
  const passthrough = useMemo(
    () =>
      new Set(
        (providerRows.data ?? [])
          .filter((p) => p.auth_mode === "oauth_passthrough")
          .map((p) => p.id),
      ),
    [providerRows.data],
  );

  // Search only: the row order *is* the routing order, so no column sort.
  const tools = useTableTools(
    deployments.data as Array<Deployment & Record<string, unknown>> | undefined,
    {
      search: (d) =>
        `${d.model_name} ${d.provider_name} ${d.upstream_name} ${d.catalog_model_id ?? ""}`,
      sort: false,
    },
  );

  // Rows in routing order, grouped by model: the first row of a group carries
  // the name, the rest read as its fallbacks. Grouping runs on the *filtered*
  // rows so a search never shows a fallback under someone else's name, and
  // only enabled backends take part in the order, as in the gateway;
  // disabled ones trail their group unranked.
  const rows = useMemo(() => {
    const filtered = [...(tools.rows ?? [])];
    const sorted = filtered.sort((a, b) => {
      if (a.model_name !== b.model_name) return routingOrder(a, b, prices);
      if (a.enabled !== b.enabled) return a.enabled ? -1 : 1;
      return routingOrder(a, b, prices);
    });
    return sorted.map((d, i) => {
      const first = i === 0 || sorted[i - 1].model_name !== d.model_name;
      let size = 1;
      for (
        let j = i + 1;
        j < sorted.length && sorted[j].model_name === d.model_name;
        j++
      )
        size++;
      let rank = d.enabled ? 1 : 0;
      for (
        let j = i - 1;
        d.enabled && j >= 0 && sorted[j].model_name === d.model_name;
        j--
      )
        rank++;
      return { ...d, _first: first, _rank: rank, _size: first ? size : 0 };
    });
  }, [tools.rows, prices]);

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<Deployment | null>(null);
  const [deleting, setDeleting] = useState<Deployment | null>(null);

  // Confirmed rather than applied on click: enabling starts sending real traffic
  // (and spend) to an upstream, disabling silently removes it from routing.
  const [toggling, setToggling] = useState<Deployment | null>(null);

  async function toggle(d: Deployment) {
    try {
      await update.mutateAsync({ id: d.id, enabled: !d.enabled });
      showToast({ body: d.enabled ? "Backend disabled" : "Backend enabled" });
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  const columns: TableColumn<Deployment & Record<string, unknown>>[] = [
    {
      key: "model_name",
      header: "Model",
      width: proportional(1.2),
      renderCell: (d) =>
        d._first ? (
          <HStack gap={1.5} vAlign="center">
            <Code>{d.model_name}</Code>
            {(d._size as number) > 1 && (
              <Badge variant="neutral" label={`${d._size} backends`} />
            )}
          </HStack>
        ) : (
          <Text type="supporting" color="secondary">
            ↳ fallback #{String(d._rank)}
          </Text>
        ),
    },
    {
      key: "provider_name",
      header: "Provider",
      width: proportional(1),
      renderCell: (d) => {
        const c = cooling.get(d.id);
        const secs = c
          ? Math.max(
              0,
              Math.round((new Date(c.until).getTime() - Date.now()) / 1000),
            )
          : 0;
        return (
          <HStack gap={1.5} vAlign="center">
            <Text type="body">{d.provider_name}</Text>
            {passthrough.has(d.provider_id) && (
              <Tooltip content="Forwards the caller's own login. Works from Claude Code, not from the Playground or workspaces.">
                <Badge variant="neutral" label="pass-through" />
              </Tooltip>
            )}
            {c && (
              <Tooltip
                content={`${c.failures} consecutive failure${c.failures === 1 ? "" : "s"} — traffic goes to the next backend; retried in ${secs}s`}
              >
                <Badge variant="warning" label={`cooling ${secs}s`} />
              </Tooltip>
            )}
          </HStack>
        );
      },
    },
    {
      key: "upstream_name",
      header: "Upstream",
      width: proportional(1.2),
      renderCell: (d) => <Code>{d.upstream_name}</Code>,
    },
    { key: "priority", header: "Priority", width: pixel(84), align: "end" },
    {
      key: "pricing",
      header: "Pricing (in/out)",
      width: proportional(1),
      renderCell: (d) => <PricingCell d={d} prices={prices} />,
    },
    {
      key: "enabled",
      header: "Enabled",
      width: pixel(88),
      renderCell: (d) => (
        <Switch
          label={`${d.enabled ? "Disable" : "Enable"} ${d.model_name}`}
          isLabelHidden
          value={d.enabled}
          changeAction={() => setToggling(d)}
        />
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(88),
      align: "end",
      renderCell: (d) => (
        <HStack gap={1} hAlign="end">
          <IconButton
            label={`Edit ${d.model_name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconEdit} size="sm" />}
            onClick={() => {
              setEditing(d);
              setShowForm(true);
            }}
          />
          <IconButton
            label={`Delete ${d.model_name}`}
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
      label="Add model"
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
      title="Models"
      description="Backends behind each model name, in routing order: priority, then cheapest. A failing backend is held out."
      action={
        <HStack gap={2} vAlign="center">
          <SearchBox
            value={tools.query}
            onChange={tools.setQuery}
            placeholder="Search models…"
          />
          {addButton}
        </HStack>
      }
    >
      <Card padding={0}>
        {deployments.isLoading ? (
          <Loading />
        ) : deployments.error ? (
          <ErrorState
            error={deployments.error}
            onRetry={() => deployments.refetch()}
          />
        ) : (deployments.data?.length ?? 0) === 0 ? (
          <Empty
            title="No models yet"
            description="Expose a model name clients can call, backed by one of your providers."
            action={addButton}
          />
        ) : (
          <Table
            data={rows}
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
          title={editing ? "Edit model" : "Add model"}
          wide
        >
          <DeploymentForm
            editing={editing}
            onClose={() => setShowForm(false)}
          />
        </FormDialog>
      )}

      <Confirm
        target={toggling}
        title={toggling?.enabled ? "Disable backend" : "Enable backend"}
        description={
          toggling?.enabled
            ? `Disable "${toggling?.model_name}" on ${toggling?.provider_name}? It stops serving traffic and routing falls through to any lower-priority deployment for that alias.`
            : `Enable "${toggling?.model_name}" on ${toggling?.provider_name}? It starts serving traffic — and incurring spend — immediately.`
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
        title="Delete model backend"
        description={`Remove "${deleting?.model_name}" on ${deleting?.provider_name}? Clients calling that name lose this backend.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Model backend deleted" });
          } catch (err) {
            showToast({
              body: err instanceof Error ? err.message : "Failed",
              type: "error",
            });
          }
          setDeleting(null);
        }}
      />
    </Page>
  );
}
