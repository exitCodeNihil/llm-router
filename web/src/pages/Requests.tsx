import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Card } from "@astryxdesign/core/Card";
import { Token } from "@astryxdesign/core/Token";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@astryxdesign/core/SegmentedControl";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { Center } from "@astryxdesign/core/Center";
import { Toolbar } from "@astryxdesign/core/Toolbar";

import type { RequestFilters, RequestRow } from "../lib/types";
import {
  useMe,
  useKeys,
  useRequests,
  useTelemetry,
  useUsageByModel,
} from "../lib/hooks";
import { compact, int, money } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { TagsInput } from "../components/forms";

type StatusOpt = "all" | "ok" | "err";

export default function Requests() {
  // Seeded from the query string so "view matching requests" on the analytics
  // page lands here with the same filters, showing the rows behind the number.
  const [sp, setSearchParams] = useSearchParams();
  const me = useMe().data;
  const [status, setStatus] = useState<StatusOpt>(
    (sp.get("status") as StatusOpt) || "all",
  );
  const [model, setModel] = useState(sp.get("model") ?? "");
  const [keyId, setKeyId] = useState(sp.get("key_id") ?? "");
  const [tags, setTags] = useState<string[]>(
    sp.get("tag") ? [sp.get("tag")!] : [],
  );
  const [search, setSearch] = useState("");
  // Passed straight through: no control for these yet, but a drill-down that
  // silently dropped them would show a different set than it promised.
  const passthrough = {
    team_id: sp.get("team_id") ?? undefined,
    provider_id: sp.get("provider_id") ?? undefined,
    error_code: sp.get("error_code") ?? undefined,
    priced: sp.get("priced") ?? undefined,
    stream: sp.get("stream") ?? undefined,
    failover: sp.get("failover") ?? undefined,
    days: sp.get("days") ?? undefined,
    request_id: sp.get("request_id") ?? undefined,
  };
  // Filters that arrived in the URL with no control of their own: shown, so
  // the page never applies something invisible, and cleared with the rest.
  const hidden = Object.entries(passthrough).filter(([, v]) => v) as [
    string,
    string,
  ][];

  // RequestFilters.tag is single-valued server-side; send the first tag only.
  const filters: RequestFilters = {
    status: status === "all" ? undefined : status,
    model: model || undefined,
    key_id: keyId || undefined,
    tag: tags[0] || undefined,
    ...passthrough,
  };

  const req = useRequests(filters);
  const tel = useTelemetry(!!me?.is_admin);
  const traceBase = tel.data?.trace_base_url;
  const models = useUsageByModel(7);
  const keys = useKeys();

  const allRows = useMemo(() => req.data?.pages.flat() ?? [], [req.data]);
  const rows = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return allRows;
    return allRows.filter(
      (r) =>
        r.model_name?.toLowerCase().includes(q) ||
        r.key_name?.toLowerCase().includes(q) ||
        r.upstream_name?.toLowerCase().includes(q),
    );
  }, [allRows, search]);

  const columns: TableColumn<RequestRow & Record<string, unknown>>[] = [
    {
      key: "ts",
      header: "Time",
      width: pixel(116),
      renderCell: (r) => <Timestamp value={r.ts} format="relative" isLive />,
    },
    {
      key: "key_name",
      header: "Key",
      width: proportional(1),
      renderCell: (r) => r.key_name ?? "—",
    },
    {
      key: "model_name",
      header: "Model → Upstream",
      width: proportional(1.6),
      renderCell: (r) => (
        <VStack gap={0}>
          <Code>{r.model_name}</Code>
          {r.upstream_name && (
            <Text type="supporting" color="secondary" maxLines={1}>
              → {r.upstream_name}
              {r.provider_name ? ` · ${r.provider_name}` : ""}
            </Text>
          )}
        </VStack>
      ),
    },
    {
      key: "latency_ms",
      header: "Latency",
      width: pixel(96),
      align: "end",
      renderCell: (r) => (
        <VStack gap={0} hAlign="end">
          <span className="tnum">{int(r.latency_ms)} ms</span>
          {!!r.ttft_ms && (
            <Text type="supporting" color="secondary">
              {int(r.ttft_ms)} ttft
            </Text>
          )}
        </VStack>
      ),
    },
    {
      key: "tokens",
      header: "Tokens",
      width: pixel(110),
      align: "end",
      renderCell: (r) => (
        <Text type="supporting" color="secondary">
          {compact(r.prompt_tokens)} / {compact(r.completion_tokens)}
        </Text>
      ),
    },
    {
      key: "cost_usd",
      header: "Cost",
      width: pixel(100),
      align: "end",
      renderCell: (r) =>
        r.cost_usd === 0 && r.notional_cost_usd ? (
          // Subscription traffic: no money changed hands, but the tokens have a
          // list value. Tilde and muted colour so it is never read as spend.
          <Text type="supporting" color="secondary">
            <span title="list value — this request cost nothing (subscription)">
              ≈{money(r.notional_cost_usd, { precise: true })}
            </span>
          </Text>
        ) : (
          money(r.cost_usd, { precise: true })
        ),
    },
    ...(traceBase
      ? [
          {
            key: "trace",
            header: "Trace",
            width: pixel(64),
            align: "end" as const,
            renderCell: (r: RequestRow) => (
              <a
                href={traceBase + r.request_id}
                target="_blank"
                rel="noreferrer"
                className="link"
                title={`Open ${r.request_id} in Langfuse`}
              >
                <Text type="supporting">open</Text>
              </a>
            ),
          },
        ]
      : []),
    {
      key: "status_code",
      header: "Status",
      // Wide enough for the longest error code: at 96px "model_not_allowed"
      // rendered as "l_not_allowed", which reads as a different failure.
      width: pixel(168),
      align: "end",
      renderCell: (r) => (
        <VStack gap={1} hAlign="end">
          <HStack gap={1} hAlign="end" vAlign="center">
            {!!r.attempts && r.attempts > 1 && (
              <Badge variant="warning" label={`${r.attempts} tries`} />
            )}
            {r.stream && <Badge variant="neutral" label="stream" />}
            <Badge
              variant={r.status_code >= 400 ? "error" : "success"}
              label={String(r.status_code)}
            />
          </HStack>
          {r.error_code && <Badge variant="error" label={r.error_code} />}
        </VStack>
      ),
    },
  ];

  const modelOptions = [
    { value: "", label: "All models" },
    // Requests refused before the body was parsed (a rate limit, say) are
    // recorded with no model, and an empty option would collide with the
    // "All models" sentinel above. Filter by outcome to find those.
    ...(models.data ?? [])
      .filter((m) => m.model_name)
      .map((m) => ({ value: m.model_name, label: m.model_name })),
  ];
  const keyOptions = [
    { value: "", label: "All keys" },
    ...(keys.data ?? []).map((k) => ({
      value: k.id,
      label: k.name || k.key_prefix,
    })),
  ];

  const hasFilters =
    status !== "all" ||
    !!model ||
    !!keyId ||
    tags.length > 0 ||
    !!search ||
    hidden.length > 0;

  return (
    <Page
      eyebrow="Overview"
      title="Requests"
      description="Every request through the gateway, newest first."
      action={
        hasFilters ? (
          <Button
            label="Clear filters"
            variant="ghost"
            onClick={() => {
              setStatus("all");
              setModel("");
              setKeyId("");
              setTags([]);
              setSearch("");
              setSearchParams({});
            }}
          />
        ) : undefined
      }
    >
      <VStack gap={4}>
        <Card>
          {/* Toolbar propagates its `size` to every child control via SizeContext,
              so the whole filter row stays visually consistent. */}
          <Toolbar
            label="Request filters"
            size="sm"
            startContent={
              <HStack gap={3} wrap="wrap" vAlign="end">
                <SegmentedControl
                  label="Status filter"
                  size="sm"
                  value={status}
                  onChange={(v) => setStatus(v as StatusOpt)}
                >
                  <SegmentedControlItem value="all" label="All" />
                  <SegmentedControlItem value="ok" label="OK" />
                  <SegmentedControlItem value="err" label="Err" />
                </SegmentedControl>
                <Selector
                  label="Model"
                  isLabelHidden
                  placeholder="All models"
                  value={model}
                  onChange={setModel}
                  options={modelOptions}
                  hasSearch={modelOptions.length > 8}
                />
                <Selector
                  label="Key"
                  isLabelHidden
                  placeholder="All keys"
                  value={keyId}
                  onChange={setKeyId}
                  options={keyOptions}
                  hasSearch={keyOptions.length > 8}
                />
              </HStack>
            }
            endContent={
              <TextInput
                label="Search loaded rows"
                isLabelHidden
                value={search}
                onChange={setSearch}
                placeholder="model, key, upstream…"
                hasClear
              />
            }
          />
          {hidden.length > 0 && (
            <HStack
              gap={1.5}
              wrap="wrap"
              vAlign="center"
              style={{ marginTop: 8 }}
            >
              <Text type="supporting" color="secondary">
                Also filtered by
              </Text>
              {hidden.map(([k, v]) => (
                <Token
                  key={k}
                  size="sm"
                  label={`${k.replace(/_/g, " ")}: ${v}`}
                  onRemove={() => {
                    const next = new URLSearchParams(sp);
                    next.delete(k);
                    setSearchParams(next);
                  }}
                />
              ))}
            </HStack>
          )}
          <VStack paddingBlock={3}>
            <TagsInput
              value={tags}
              onChange={setTags}
              label="Tag"
              description="Server-side filter (first tag)"
            />
          </VStack>
        </Card>

        <Card padding={0}>
          {req.isLoading ? (
            <Loading />
          ) : req.error ? (
            <ErrorState error={req.error} onRetry={() => req.refetch()} />
          ) : rows.length === 0 ? (
            <Empty
              title="No requests match"
              description="Adjust the filters, or route some traffic through the gateway."
            />
          ) : (
            <VStack gap={0}>
              <Table
                data={rows as Array<RequestRow & Record<string, unknown>>}
                columns={columns}
                idKey="id"
                density="compact"
                dividers="rows"
                hasHover
                textOverflow="truncate"
              />
              {req.hasNextPage && (
                <Center>
                  <VStack padding={4}>
                    <Button
                      label={req.isFetchingNextPage ? "Loading…" : "Load more"}
                      variant="secondary"
                      isLoading={req.isFetchingNextPage}
                      onClick={() => req.fetchNextPage()}
                    />
                  </VStack>
                </Center>
              )}
            </VStack>
          )}
        </Card>
      </VStack>
    </Page>
  );
}
