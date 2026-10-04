import { Badge } from "@astryxdesign/core/Badge";
import { Card } from "@astryxdesign/core/Card";
import { Code } from "@astryxdesign/core/Code";
import { HStack } from "@astryxdesign/core/Stack";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { Tooltip } from "@astryxdesign/core/Tooltip";

import type { AvailableModel } from "../lib/types";
import { useAvailableModels } from "../lib/hooks";
import { money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { SearchBox, useTableTools } from "../components/tables";

const PROVIDER_LABEL: Record<string, string> = {
  azure: "Azure",
  gcp_vertex: "Vertex AI",
  openrouter: "OpenRouter",
  openai_compatible: "OpenAI-compatible",
};

/**
 * What a member sees under Models: the names they can put in a client,
 * what each costs per million tokens, and whether it is answering. Routing
 * details stay on the admin page.
 */
export default function ModelsAvailable() {
  const models = useAvailableModels();

  const columns: TableColumn<AvailableModel & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Model",
      width: proportional(2.8),
      sortable: true,
      renderCell: (m) => (
        <HStack gap={2} vAlign="center">
          <Code>{m.name}</Code>
          {m.passthrough && (
            <Tooltip content="Forwards your own subscription token. Works from Claude Code with the subscription setup; not from the Playground or a plain API key.">
              <Badge variant="neutral" label="subscription" />
            </Tooltip>
          )}
        </HStack>
      ),
    },
    {
      key: "api_flavor",
      header: "Protocol",
      width: pixel(110),
      renderCell: (m) => (
        <Text type="supporting" color="secondary">
          {m.api_flavor === "anthropic" ? "Anthropic" : "OpenAI"}
        </Text>
      ),
    },
    {
      key: "input_per_1m",
      header: "Price / 1M tokens",
      width: proportional(1.4),
      sortable: true,
      renderCell: (m) =>
        m.input_per_1m == null || m.output_per_1m == null ? (
          <Text type="supporting" color="secondary">
            {m.passthrough ? "your subscription" : "unpriced"}
          </Text>
        ) : (
          <Text type="body" className="tnum">
            {money(m.input_per_1m)} in · {money(m.output_per_1m)} out
          </Text>
        ),
    },
    {
      key: "via",
      header: "Runs on",
      width: proportional(1),
      renderCell: (m) => (
        <Text type="supporting" color="secondary">
          {(m.via ?? []).map((v) => PROVIDER_LABEL[v] ?? v).join(", ") || "—"}
          {m.backends > 1 ? ` · ${m.backends} backends` : ""}
        </Text>
      ),
    },
    {
      key: "status",
      header: "Status",
      width: pixel(110),
      sortable: true,
      renderCell: (m) =>
        m.status === "ok" ? (
          <Badge variant="success" label="available" />
        ) : m.status === "degraded" ? (
          <Tooltip content="One backend is cooling down after errors; requests fail over to another.">
            <Badge variant="warning" label="degraded" />
          </Tooltip>
        ) : (
          <Tooltip content="Every backend is cooling down after errors. Try again in a minute.">
            <Badge variant="error" label="down" />
          </Tooltip>
        ),
    },
  ];

  const tools = useTableTools(
    models.data as Array<AvailableModel & Record<string, unknown>> | undefined,
    {
      search: (m) => `${m.name} ${(m.via ?? []).join(" ")} ${m.api_flavor}`,
      defaultSort: [{ sortKey: "name", direction: "ascending" }],
    },
  );

  return (
    <Page
      eyebrow="Develop"
      title="Models"
      description="Every model this gateway serves. Use the name as the model id in any client; a key may be limited to some of them."
      action={
        <SearchBox
          value={tools.query}
          onChange={tools.setQuery}
          placeholder="Search models…"
        />
      }
    >
      <Card padding={0}>
        {models.isLoading ? (
          <Loading />
        ) : models.error ? (
          <ErrorState error={models.error} onRetry={() => models.refetch()} />
        ) : (models.data?.length ?? 0) === 0 ? (
          <Empty
            title="No models yet"
            description="An admin has not added any models to the gateway."
          />
        ) : (
          <Table
            data={tools.rows}
            plugins={tools.plugins}
            columns={columns}
            idKey="name"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </Card>
    </Page>
  );
}
