import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { Icon } from "@astryxdesign/core/Icon";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { NumberInput } from "@astryxdesign/core/NumberInput";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { Price } from "../lib/types";
import { usePrices, useSetPrice } from "../lib/hooks";
import { money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { FormActions, FormDialog } from "../components/dialogs";
import { IconPlus } from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";
import { api } from "../lib/api";

function PriceForm({ price, onDone }: { price: Price | null; onDone: () => void }) {
  const set = useSetPrice();
  const showToast = useToast();
  const isNew = !price;

  const [modelId, setModelId] = useState(price?.model_id ?? "");
  const [input, setInput] = useState<number | null>(price?.input_per_1m ?? null);
  const [output, setOutput] = useState<number | null>(price?.output_per_1m ?? null);
  const [cached, setCached] = useState<number | null>(price?.cached_input_per_1m ?? null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await set.mutateAsync({
        model_id: modelId.trim(),
        input_per_1m: input ?? 0,
        output_per_1m: output ?? 0,
        // null keeps "no cached rate"; 0 would mean cache reads are free
        cached_input_per_1m: cached,
      });
      showToast({ body: "Price saved" });
      onDone();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Model id"
          value={modelId}
          onChange={setModelId}
          placeholder="azure/gpt-4o-2024-08-06"
          description="Referenced by a deployment's catalog_model_id"
          isRequired
          isDisabled={!isNew}
          disabledMessage="Model id is the catalog key and cannot be renamed"
          hasAutoFocus={isNew}
        />
        <Grid columns={{ minWidth: 150, repeat: "fit" }} gap={3}>
          <NumberInput label="Input $/1M" value={input} onChange={setInput} min={0} step={0.01} placeholder="0.00" />
          <NumberInput label="Output $/1M" value={output} onChange={setOutput} min={0} step={0.01} placeholder="0.00" />
          <NumberInput
            label="Cached input $/1M"
            value={cached}
            onChange={setCached}
            min={0}
            step={0.01}
            placeholder="0.00"
          />
        </Grid>
        <FormActions
          onCancel={onDone}
          submitLabel="Save price"
          isLoading={set.isPending}
          isDisabled={!modelId.trim()}
        />
      </VStack>
    </form>
  );
}

export default function Pricing() {
  const prices = usePrices();
  const [editing, setEditing] = useState<Price | null>(null);
  const [creating, setCreating] = useState(false);
  const tools = useTableTools(prices.data as Array<Price & Record<string, unknown>> | undefined, {
    search: (p) => `${p.model_id} ${p.source}`,
    defaultSort: [{ sortKey: "model_id", direction: "ascending" }],
  });
  const showToast = useToast();

  const columns: TableColumn<Price & Record<string, unknown>>[] = [
    {
      key: "model_id",
      header: "Model id",
      sortable: true,
      width: proportional(2),
      renderCell: (p) => <Code>{p.model_id}</Code>,
    },
    {
      key: "input_per_1m",
      header: "Input $/1M",
      sortable: true,
      width: pixel(120),
      align: "end",
      renderCell: (p) => money(p.input_per_1m, { precise: true }),
    },
    {
      key: "output_per_1m",
      header: "Output $/1M",
      sortable: true,
      width: pixel(120),
      align: "end",
      renderCell: (p) => money(p.output_per_1m, { precise: true }),
    },
    {
      key: "cached_input_per_1m",
      header: "Cached $/1M",
      sortable: true,
      width: pixel(124),
      align: "end",
      renderCell: (p) =>
        p.cached_input_per_1m ? (
          money(p.cached_input_per_1m, { precise: true })
        ) : (
          <Text type="supporting" color="disabled">
            —
          </Text>
        ),
    },
    {
      key: "source",
      header: "Source",
      sortable: true,
      width: pixel(96),
      renderCell: (p) => (
        <Badge variant={p.source === "admin" ? "purple" : "neutral"} label={p.source} />
      ),
    },
    {
      key: "updated_at",
      header: "Updated",
      sortable: true,
      width: pixel(120),
      renderCell: (p) => <Timestamp value={p.updated_at} format="relative" />,
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(80),
      align: "end",
      renderCell: (p) => <Button label="Edit" variant="ghost" size="sm" onClick={() => setEditing(p)} />,
    },
  ];

  return (
    <Page
      eyebrow="Routing"
      title="Price catalog"
      description="List prices per 1M tokens. Azure and Google rows ship built in; OpenRouter rows sync from its API. Edited rows keep your value."
      action={
        <HStack gap={2} vAlign="center" wrap="wrap">
          <SearchBox value={tools.query} onChange={tools.setQuery} placeholder="Search models…" />
          <Button
            label="Sync OpenRouter"
            variant="secondary"
            clickAction={async () => {
              try {
                const r = await api.post<{ providers: number; models: number }>("/api/prices/sync", {});
                await prices.refetch();
                showToast({
                  body: r.providers
                    ? `Synced ${r.models} OpenRouter prices`
                    : "No OpenRouter provider configured",
                });
              } catch (err) {
                showToast({ body: err instanceof Error ? err.message : "Sync failed", type: "error" });
              }
            }}
          />
          <Button
            label="Add price"
            variant="primary"
            icon={<Icon icon={IconPlus} size="sm" />}
            onClick={() => setCreating(true)}
          />
        </HStack>
      }
    >
      <Card padding={0}>
        {prices.isLoading ? (
          <Loading />
        ) : prices.error ? (
          <ErrorState error={prices.error} onRetry={() => prices.refetch()} />
        ) : tools.rows.length === 0 ? (
          <Empty
            title={tools.query ? "No models match" : "Catalog is empty"}
            description={tools.query ? "Try a different search." : "Add a price to charge for a custom model."}
          />
        ) : (
          <Table
            data={tools.rows}
            plugins={tools.plugins}
            columns={columns}
            idKey="model_id"
            density="compact"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </Card>

      {(editing || creating) && (
        <FormDialog
          isOpen
          onClose={() => {
            setEditing(null);
            setCreating(false);
          }}
          title={editing ? "Edit price" : "Add price"}
          wide
        >
          <PriceForm
            price={editing}
            onDone={() => {
              setEditing(null);
              setCreating(false);
            }}
          />
        </FormDialog>
      )}
    </Page>
  );
}
