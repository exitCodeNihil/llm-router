import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
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

import type { AuditEvent, AuditFilters } from "../lib/types";
import { useAudit } from "../lib/hooks";
import { dateTime } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { FormDialog } from "../components/dialogs";

type StatusOpt = "all" | "ok" | "err";

const TARGET_TYPES: [string, string][] = [
  ["user", "User"],
  ["team", "Team"],
  ["team_member", "Team member"],
  ["key", "API key"],
  ["provider", "Provider"],
  ["deployment", "Deployment"],
  ["price", "Price"],
  ["setting", "Setting"],
  ["workspace", "Workspace"],
  ["workspace_template", "Workspace template"],
  ["workspace_files", "Workspace files"],
  ["gateway_group", "Gateway group"],
  ["gateway_instance", "Gateway instance"],
  ["token_issuer", "Token issuer"],
  ["telemetry_destination", "Telemetry destination"],
];

const NAMED: Record<string, string> = {
  "auth.signin": "Signed in",
  "auth.signin_failed": "Failed sign-in",
  "auth.sso_signin": "Signed in with SSO",
  "auth.sso_signin_failed": "Failed SSO sign-in",
  "auth.signout": "Signed out",
  "auth.setup": "Created the first admin",
  "POST /api/me/password": "Change own password",
  "POST /api/users/{id}/password": "Set a user's password",
  "POST /api/teams/{id}/members": "Add team member",
  "PATCH /api/teams/{id}/members/{uid}": "Update team member",
  "DELETE /api/teams/{id}/members/{uid}": "Remove team member",
  "PUT /api/prices/{id...}": "Set price",
  "POST /api/prices/sync": "Sync prices",
  "PUT /api/settings/sso": "Change SSO settings",
  "PUT /api/settings/telemetry": "Change capture policy",
  "PUT /api/settings/workspaces": "Change workspace settings",
  "POST /api/workspace-templates/{id}/build": "Build workspace template",
  "PUT /api/workspaces/files": "Upload workspace files",
  "DELETE /api/workspaces/files": "Delete workspace file",
  "POST /api/workspaces/{id}/start": "Start workspace",
  "POST /api/workspaces/{id}/stop": "Stop workspace",
  "POST /api/workspaces/{id}/upload": "Upload files to workspace",
  "POST /api/gateway-groups/{id}/rotate-token": "Rotate join token",
  "POST /api/telemetry/destinations/test": "Test telemetry destination",
};

const VERBS: Record<string, string> = {
  POST: "Create",
  PUT: "Replace",
  PATCH: "Update",
  DELETE: "Delete",
};

const NOUNS: Record<string, string> = {
  users: "user",
  teams: "team",
  keys: "API key",
  providers: "provider",
  deployments: "deployment",
  "gateway-groups": "gateway group",
  "gateway-instances": "gateway instance",
  "token-issuers": "token issuer",
  "workspace-templates": "workspace template",
  workspaces: "workspace",
  telemetry: "telemetry destination",
};

/** "DELETE /api/users/{id}" → "Delete user"; the raw pattern stays visible. */
function actionLabel(action: string): string {
  if (NAMED[action]) return NAMED[action];
  const [method, path = ""] = action.split(" ");
  const noun = NOUNS[path.replace(/^\/api\//, "").split("/")[0]];
  return VERBS[method] && noun ? `${VERBS[method]} ${noun}` : action;
}

function actor(e: AuditEvent): string {
  if (e.actor_kind === "admin_token") return "admin token";
  if (e.actor_kind === "anonymous") return "anonymous";
  return e.actor_email ?? e.actor_user_id ?? "user";
}

function target(e: AuditEvent): string {
  if (!e.target_type) return "—";
  const label = TARGET_TYPES.find(([v]) => v === e.target_type)?.[1] ?? e.target_type;
  return e.target_id ? `${label} ${e.target_id}` : label;
}

function StatusBadge({ status }: { status: number }) {
  return (
    <Badge
      variant={status >= 400 ? "error" : "success"}
      label={`${status >= 400 ? "err" : "ok"} ${status}`}
    />
  );
}

function EventDetail({
  event: e,
  onClose,
  onFilterAction,
}: {
  event: AuditEvent | null;
  onClose: () => void;
  onFilterAction: (action: string) => void;
}) {
  if (!e) return null;
  const fields: [string, ReactNode][] = [
    ["Time", dateTime(e.ts)],
    ["Actor", actor(e)],
    [
      "Action",
      <span key="a">
        {actionLabel(e.action)} <Code>{e.action}</Code>
      </span>,
    ],
    ["Target", target(e)],
    ["Status", <StatusBadge key="s" status={e.status} />],
    ["IP", e.client_ip ?? "—"],
  ];
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      wide
      title="Audit event"
      subtitle="Secrets, passwords and personal fields are never recorded."
    >
      <VStack gap={2}>
        {fields.map(([label, value]) => (
          <HStack key={label} gap={3} vAlign="start">
            <div style={{ width: 96, flexShrink: 0 }}>
              <Text type="supporting" color="secondary">
                {label}
              </Text>
            </div>
            <div style={{ minWidth: 0, overflowWrap: "anywhere" }}>
              <Text type="body">{value}</Text>
            </div>
          </HStack>
        ))}
      </VStack>
      {e.detail ? (
        <CodeBlock code={JSON.stringify(e.detail, null, 2)} language="json" width="100%" maxHeight={320} isWrapped />
      ) : (
        <Text type="supporting" color="secondary">
          No request detail is kept for this action.
        </Text>
      )}
      <HStack gap={2} hAlign="end" wrap="wrap">
        <Button
          label="Show only this action"
          variant="secondary"
          onClick={() => {
            onFilterAction(e.action);
            onClose();
          }}
        />
        <Button label="Close" variant="ghost" onClick={onClose} />
      </HStack>
    </FormDialog>
  );
}

export default function Audit() {
  const [status, setStatus] = useState<StatusOpt>("all");
  const [targetType, setTargetType] = useState("");
  const [actionInput, setActionInput] = useState("");
  const [action, setAction] = useState("");
  const [detail, setDetail] = useState<AuditEvent | null>(null);

  // The server matches the action exactly; wait for typing to stop.
  useEffect(() => {
    const t = setTimeout(() => setAction(actionInput.trim()), 400);
    return () => clearTimeout(t);
  }, [actionInput]);

  const filters: AuditFilters = {
    status: status === "all" ? undefined : status,
    target_type: targetType || undefined,
    action: action || undefined,
  };
  const q = useAudit(filters);
  const rows = useMemo(() => q.data?.pages.flat() ?? [], [q.data]);

  const columns: TableColumn<AuditEvent & Record<string, unknown>>[] = [
    {
      key: "ts",
      header: "Time",
      width: pixel(116),
      renderCell: (e) => (
        <button type="button" className="rowlink" title="Show what was recorded" onClick={() => setDetail(e)}>
          <Timestamp value={e.ts} format="relative" isLive />
        </button>
      ),
    },
    {
      key: "actor",
      header: "Actor",
      width: proportional(1.2),
      renderCell: (e) =>
        e.actor_kind === "user" ? (
          actor(e)
        ) : (
          <Text type="supporting" color="secondary">
            {actor(e)}
          </Text>
        ),
    },
    {
      key: "action",
      header: "Action",
      width: proportional(1.6),
      renderCell: (e) => (
        <VStack gap={0}>
          <span>{actionLabel(e.action)}</span>
          <Text type="supporting" color="secondary" maxLines={1}>
            {e.action}
          </Text>
        </VStack>
      ),
    },
    {
      key: "target",
      header: "Target",
      width: proportional(1.4),
      renderCell: (e) => (
        <Text type="supporting" maxLines={1}>
          {target(e)}
        </Text>
      ),
    },
    {
      key: "status",
      header: "Status",
      width: pixel(96),
      align: "end",
      renderCell: (e) => <StatusBadge status={e.status} />,
    },
    {
      key: "client_ip",
      header: "IP",
      width: pixel(128),
      align: "end",
      renderCell: (e) => (
        <Text type="supporting" color="secondary">
          {e.client_ip ?? "—"}
        </Text>
      ),
    },
  ];

  const hasFilters = status !== "all" || !!targetType || !!actionInput;

  return (
    <Page
      eyebrow="Operations"
      title="Audit log"
      description="Every change made through the console or the management API, sign-ins included, newest first. Refused attempts are recorded too."
      action={
        hasFilters ? (
          <Button
            label="Clear filters"
            variant="ghost"
            onClick={() => {
              setStatus("all");
              setTargetType("");
              setActionInput("");
            }}
          />
        ) : undefined
      }
    >
      <EventDetail event={detail} onClose={() => setDetail(null)} onFilterAction={setActionInput} />
      <VStack gap={4}>
        <Card>
          <Toolbar
            label="Audit filters"
            size="sm"
            startContent={
              <HStack gap={3} wrap="wrap" vAlign="end">
                <SegmentedControl
                  label="Outcome filter"
                  size="sm"
                  value={status}
                  onChange={(v) => setStatus(v as StatusOpt)}
                >
                  <SegmentedControlItem value="all" label="All" />
                  <SegmentedControlItem value="ok" label="OK" />
                  <SegmentedControlItem value="err" label="Err" />
                </SegmentedControl>
                <Selector
                  label="Target type"
                  isLabelHidden
                  placeholder="All targets"
                  value={targetType}
                  onChange={setTargetType}
                  options={[
                    { value: "", label: "All targets" },
                    ...TARGET_TYPES.map(([value, label]) => ({ value, label })),
                  ]}
                  hasSearch
                />
              </HStack>
            }
            endContent={
              <TextInput
                label="Action"
                isLabelHidden
                value={actionInput}
                onChange={setActionInput}
                placeholder="exact action, e.g. DELETE /api/keys/{id}"
                hasClear
              />
            }
          />
        </Card>

        <Card padding={0}>
          {q.isLoading ? (
            <Loading />
          ) : q.error ? (
            <ErrorState error={q.error} onRetry={() => q.refetch()} />
          ) : rows.length === 0 ? (
            <Empty
              title="No events match"
              description={hasFilters ? "Adjust the filters." : "Changes made in the console or through the API appear here."}
            />
          ) : (
            <VStack gap={0}>
              <Table
                data={rows as Array<AuditEvent & Record<string, unknown>>}
                columns={columns}
                idKey="id"
                density="compact"
                dividers="rows"
                hasHover
                textOverflow="truncate"
              />
              {q.hasNextPage && (
                <Center>
                  <VStack padding={4}>
                    <Button
                      label={q.isFetchingNextPage ? "Loading…" : "Load more"}
                      variant="secondary"
                      isLoading={q.isFetchingNextPage}
                      onClick={() => q.fetchNextPage()}
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
