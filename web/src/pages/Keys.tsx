import { useMemo, useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Badge } from "@astryxdesign/core/Badge";
import { Switch } from "@astryxdesign/core/Switch";
import { Icon } from "@astryxdesign/core/Icon";
import { Code } from "@astryxdesign/core/Code";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { MultiSelector } from "@astryxdesign/core/MultiSelector";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { useToast } from "@astryxdesign/core/Toast";

import type { ApiKey, Me, TelemetrySettings } from "../lib/types";
import {
  useChatModels,
  useCreateKey,
  useDeleteKey,
  useDeployments,
  useKeys,
  useTeams,
  useTelemetry,
  useUpdateKey,
  useUsers,
} from "../lib/hooks";
import { budgetLabel } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import { ConnectDialog } from "../components/connect";
import {
  ModelsCell,
  limitsInvalid,
  LimitsFields,
  TagChips,
  TagsInput,
  emptyLimits,
  limitsFrom,
  limitsPayload,
  type LimitsState,
} from "../components/forms";
import {
  IconEdit,
  IconPlus,
  IconTerminal,
  IconTrash,
} from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";

type OwnerType = "self" | "team" | "user";

/** What key creation hands back: the secret and the models it was scoped to. */
type Created = { key: string; models: string[] | null };

function ownerLabel(k: ApiKey, teamName?: string, userEmail?: string): string {
  // A member's team key names both: the team pays, and so does the member.
  if (k.team_id && k.user_id)
    return `Team · ${teamName ?? "?"} · ${userEmail ?? "member"}`;
  if (k.team_id) return teamName ? `Team · ${teamName}` : "Team";
  if (k.user_id) return userEmail ?? "User";
  return "—";
}

/** Whether telemetry export applies to this key under the current settings. */
function evalTrace(
  k: ApiKey,
  t: TelemetrySettings["telemetry"],
): { capture: "content" | "meta" | "none"; rule?: string } {
  if (!t || t.mode === "off") return { capture: "none" };
  if (t.mode !== "by_rule")
    return { capture: t.capture_content ? "content" : "meta" };
  const rule = t.rules
    .filter((r) => r.enabled)
    .sort((a, b) => a.order - b.order)
    .find((r) =>
      r.scope_type === "key"
        ? r.scope_value === k.id
        : r.scope_type === "team"
          ? r.scope_value === k.team_id
          : r.scope_type === "user"
            ? r.scope_value === k.user_id
            : (k.tags?.includes(r.scope_value) ?? false),
    );
  return rule ? { capture: rule.capture, rule: rule.id } : { capture: "none" };
}

function TraceBadge({
  capture,
  rule,
}: {
  capture: "content" | "meta" | "none";
  rule?: string;
}) {
  if (capture === "none")
    return (
      <Text type="supporting" color="secondary">
        off
      </Text>
    );
  const label = capture === "content" ? "content" : "meta";
  const badge = (
    <Badge
      variant={capture === "content" ? "purple" : "neutral"}
      label={label}
    />
  );
  return rule ? (
    <Tooltip content={`Matched rule ${rule}`}>{badge}</Tooltip>
  ) : (
    badge
  );
}

// ── Create / edit form ──────────────────────────────────────────
function KeyForm({
  me,
  editing,
  onClose,
  onSecret,
}: {
  me: Me;
  editing: ApiKey | null;
  onClose: () => void;
  onSecret: (s: Created) => void;
}) {
  const create = useCreateKey();
  const update = useUpdateKey();
  const showToast = useToast();
  const teams = useTeams();
  const users = useUsers(me.is_admin);
  // Admins pick from the routing table; everyone else from the alias list
  // the playground uses, which is what a member's key can call anyway.
  const deployments = useDeployments(me.is_admin);
  const chatModels = useChatModels();

  const canSelf = Boolean(me.user_id);
  const [name, setName] = useState(editing?.name ?? "");
  const [ownerType, setOwnerType] = useState<OwnerType>(
    editing?.team_id
      ? "team"
      : editing?.user_id && editing.user_id !== me.user_id
        ? "user"
        : canSelf
          ? "self"
          : me.is_admin
            ? "user"
            : "team",
  );
  const [teamId, setTeamId] = useState(editing?.team_id ?? "");
  const [userId, setUserId] = useState(editing?.user_id ?? "");
  const [selectedModels, setSelectedModels] = useState<string[]>(
    editing?.allowed_models ?? [],
  );
  const [expires, setExpires] = useState(
    editing?.expires_at?.slice(0, 10) ?? "",
  );
  const [tags, setTags] = useState<string[]>(editing?.tags ?? []);
  const [limits, setLimits] = useState<LimitsState>(
    editing ? limitsFrom(editing) : emptyLimits,
  );

  const aliases = useMemo(
    () =>
      me.is_admin
        ? [...new Set((deployments.data ?? []).map((d) => d.model_name))].sort()
        : [...(chatModels.data ?? [])].sort(),
    [deployments.data, chatModels.data, me.is_admin],
  );
  // Budgets and rate limits on a key are set by an admin, or by the admin of
  // the team that owns it; a member capping their own key is no cap.
  const limitsTeam = editing
    ? editing.team_id
    : ownerType === "team"
      ? teamId
      : null;
  const canSetLimits =
    me.is_admin || (!!limitsTeam && me.team_roles[limitsTeam] === "admin");
  const expiresInvalid = !!expires && Number.isNaN(new Date(expires).getTime());

  // Admins may mint keys for any team; everyone else for teams they belong to
  // (the key then spends against the team and against them).
  const adminTeams = useMemo(
    () =>
      (teams.data ?? []).filter((t) => me.is_admin || t.id in me.team_roles),
    [teams.data, me],
  );

  const busy = create.isPending || update.isPending;

  const ownerOptions = [
    ...(canSelf ? [{ value: "self", label: "Just me" }] : []),
    ...(adminTeams.length ? [{ value: "team", label: "A team I'm in" }] : []),
    ...(me.is_admin ? [{ value: "user", label: "Another user" }] : []),
  ];

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      const base = {
        name: name.trim(),
        // null (not undefined) so clearing the selection on edit clears in the DB
        allowed_models: selectedModels.length ? selectedModels : null,
        tags,
        // null so that clearing the date on edit actually clears it
        expires_at: expires ? new Date(expires).toISOString() : null,
        ...(canSetLimits ? limitsPayload(limits) : {}),
      };
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...base });
        showToast({ body: "Key updated" });
      } else {
        const owner =
          ownerType === "team"
            ? { team_id: teamId }
            : ownerType === "user"
              ? { user_id: userId }
              : { user_id: me.user_id };
        if (!owner.team_id && !owner.user_id) {
          showToast({ body: "Choose an owner for the key", type: "error" });
          return;
        }
        const res = await create.mutateAsync({ ...base, ...owner });
        onSecret({
          key: (res as { key: string }).key,
          models: base.allowed_models,
        });
      }
      onClose();
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed to save key",
        type: "error",
      });
    }
  }

  const ownerMissing =
    !editing &&
    ((ownerType === "team" && !teamId) || (ownerType === "user" && !userId));

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          placeholder="e.g. production-backend"
          isRequired
          hasAutoFocus
        />

        {!editing && (
          <Selector
            label="Spends against"
            description="Whose budget this key draws on. A team key also counts against you and uses the team's models."
            value={ownerType}
            onChange={(v) => setOwnerType(v as OwnerType)}
            options={ownerOptions}
          />
        )}

        {!editing && ownerType === "team" && (
          <Selector
            label="Team"
            value={teamId}
            onChange={setTeamId}
            placeholder="Select a team…"
            isRequired
            hasSearch={adminTeams.length > 8}
            options={adminTeams.map((t) => ({ value: t.id, label: t.name }))}
          />
        )}

        {!editing && ownerType === "user" && (
          <Selector
            label="User"
            value={userId}
            onChange={setUserId}
            placeholder="Select a user…"
            isRequired
            hasSearch
            options={(users.data ?? []).map((u) => ({
              value: u.id,
              label: u.email,
            }))}
          />
        )}

        {aliases.length > 0 ? (
          <MultiSelector
            label="Allowed models"
            value={selectedModels}
            onChange={setSelectedModels}
            options={aliases.map((a) => ({ value: a, label: a }))}
            placeholder="All models"
            triggerDisplay="badges"
            hasSearch={aliases.length > 8}
            hasSelectAll
            description="None selected = access to every model"
          />
        ) : (
          <TextInput
            label="Allowed models"
            value={selectedModels.join(", ")}
            onChange={(v) =>
              setSelectedModels(
                v
                  .split(",")
                  .map((s) => s.trim())
                  .filter(Boolean),
              )
            }
            placeholder="gpt-4o, claude-sonnet-5 (comma-separated)"
            description="No models yet — enter model names manually"
          />
        )}

        {canSetLimits ? (
          <LimitsFields value={limits} onChange={setLimits} />
        ) : (
          <Text type="supporting" color="secondary">
            Budget and rate limits come from your account
            {limitsTeam ? " and team" : ""}; an admin sets them.
          </Text>
        )}

        <TextInput
          label="Expires"
          type="text"
          value={expires}
          onChange={setExpires}
          placeholder="YYYY-MM-DD"
          isOptional
          description="Blank = never expires"
          status={
            expires && Number.isNaN(new Date(expires).getTime())
              ? { type: "error", message: "Use YYYY-MM-DD" }
              : undefined
          }
        />

        <TagsInput
          value={tags}
          onChange={setTags}
          description="Group and scope by label, e.g. prod, debug"
        />

        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Create key"}
          isLoading={busy}
          isDisabled={
            !name.trim() ||
            ownerMissing ||
            expiresInvalid ||
            (canSetLimits && limitsInvalid(limits))
          }
        />
      </VStack>
    </form>
  );
}

// ── Page ────────────────────────────────────────────────────────
export default function Keys({ me }: { me: Me }) {
  const keys = useKeys();
  const teams = useTeams();
  const users = useUsers(me.is_admin);
  const del = useDeleteKey();
  const update = useUpdateKey();
  const showToast = useToast();
  const telemetry = useTelemetry(me.is_admin).data?.telemetry;
  // Mirrors canManageKey server-side: admins, the owning team's admins, and
  // the personal owner. Everyone else gets a read-only row.
  const canManage = (k: ApiKey) =>
    me.is_admin ||
    (k.team_id
      ? me.team_roles[k.team_id] === "admin"
      : k.user_id === me.user_id);

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<ApiKey | null>(null);
  const [secret, setSecret] = useState<Created | null>(null);
  const [connecting, setConnecting] = useState<ApiKey | null>(null);
  const [deleting, setDeleting] = useState<ApiKey | null>(null);

  const teamName = (id: string | null) =>
    teams.data?.find((t) => t.id === id)?.name;
  const userEmail = (id: string | null) =>
    users.data?.find((u) => u.id === id)?.email;

  const openCreate = () => {
    setEditing(null);
    setShowForm(true);
  };

  // Confirmed rather than applied on click: one stray tap on a row switch
  // either cuts off a live integration or hands access back.
  const [toggling, setToggling] = useState<ApiKey | null>(null);

  async function toggleDisabled(k: ApiKey) {
    try {
      await update.mutateAsync({ id: k.id, disabled: !k.disabled });
      showToast({ body: k.disabled ? "Key enabled" : "Key disabled" });
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  const columns: TableColumn<ApiKey & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Name",
      sortable: true,
      width: proportional(1.4),
      renderCell: (k) => (
        <VStack gap={0}>
          <Text type="body" weight="medium" maxLines={1}>
            {k.name || "—"}
          </Text>
          <Code>{k.key_prefix}…</Code>
        </VStack>
      ),
    },
    {
      key: "owner",
      header: "Owner",
      width: proportional(1),
      renderCell: (k) =>
        ownerLabel(k, teamName(k.team_id), userEmail(k.user_id)),
    },
    {
      key: "allowed_models",
      header: "Models",
      width: pixel(96),
      renderCell: (k) => <ModelsCell models={k.allowed_models} />,
    },
    {
      key: "tags",
      header: "Tags",
      width: proportional(1),
      renderCell: (k) => <TagChips tags={k.tags} />,
    },
    {
      key: "trace",
      header: "Trace",
      width: pixel(96),
      renderCell: (k) => <TraceBadge {...evalTrace(k, telemetry)} />,
    },
    {
      key: "budget_usd",
      sortable: true,
      header: "Budget",
      width: pixel(120),
      renderCell: (k) => budgetLabel(k.budget_usd, k.budget_period),
    },
    {
      key: "expires_at",
      sortable: true,
      header: "Expires",
      width: pixel(110),
      renderCell: (k) =>
        k.expires_at ? (
          <Timestamp value={k.expires_at} format="date" />
        ) : (
          "Never"
        ),
    },
    {
      key: "disabled",
      header: "Status",
      width: pixel(92),
      // The switch *is* the status for anyone who may flip it; a badge for
      // everyone else. Keeping it out of the actions cell keeps that cell
      // narrow enough to stay on screen at laptop widths.
      renderCell: (k) =>
        canManage(k) ? (
          <Switch
            label={k.disabled ? `Enable ${k.name}` : `Disable ${k.name}`}
            isLabelHidden
            value={!k.disabled}
            changeAction={() => setToggling(k)}
          />
        ) : k.disabled ? (
          <Badge variant="error" label="disabled" />
        ) : (
          <Badge variant="success" label="active" />
        ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(120),
      align: "end",
      renderCell: (k) => (
        <HStack gap={1} vAlign="center" hAlign="end">
          <IconButton
            label={`Connect a client with ${k.name}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTerminal} size="sm" />}
            onClick={() => setConnecting(k)}
          />
          {canManage(k) && (
            <>
              <IconButton
                label={`Edit ${k.name}`}
                variant="ghost"
                size="sm"
                icon={<Icon icon={IconEdit} size="sm" />}
                onClick={() => {
                  setEditing(k);
                  setShowForm(true);
                }}
              />
              <IconButton
                label={`Delete ${k.name}`}
                variant="ghost"
                size="sm"
                icon={<Icon icon={IconTrash} size="sm" />}
                onClick={() => setDeleting(k)}
              />
            </>
          )}
        </HStack>
      ),
    },
  ];

  // Trace policy is admin data; members would only ever see "off".
  const visibleColumns = me.is_admin
    ? columns
    : columns.filter((c) => c.key !== "trace");

  const tools = useTableTools(
    keys.data as Array<ApiKey & Record<string, unknown>> | undefined,
    {
      search: (k) =>
        `${k.name} ${k.key_prefix} ${ownerLabel(k, teamName(k.team_id), userEmail(k.user_id))} ${(k.tags ?? []).join(" ")}`,
      defaultSort: [{ sortKey: "name", direction: "ascending" }],
    },
  );

  const newKeyButton = (
    <Button
      label="New key"
      variant="primary"
      icon={<Icon icon={IconPlus} size="sm" />}
      onClick={openCreate}
    />
  );

  return (
    <Page
      eyebrow="Access"
      title="API Keys"
      description="Virtual keys clients use to call the gateway. Scope models, budgets, and rate limits per key."
      action={
        <HStack gap={2} vAlign="center">
          <SearchBox
            value={tools.query}
            onChange={tools.setQuery}
            placeholder="Search keys…"
          />
          {newKeyButton}
        </HStack>
      }
    >
      <Card padding={0}>
        {keys.isLoading ? (
          <Loading />
        ) : keys.error ? (
          <ErrorState error={keys.error} onRetry={() => keys.refetch()} />
        ) : (keys.data?.length ?? 0) === 0 ? (
          <Empty
            title="No API keys yet"
            description="Create a key to let a client call the gateway with scoped budgets and limits."
            action={newKeyButton}
          />
        ) : (
          <Table
            data={tools.rows}
            plugins={tools.plugins}
            columns={visibleColumns}
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
          title={editing ? "Edit key" : "Create API key"}
          subtitle={
            editing
              ? undefined
              : "The full secret is shown once after creation."
          }
          wide
        >
          <KeyForm
            me={me}
            editing={editing}
            onClose={() => setShowForm(false)}
            onSecret={setSecret}
          />
        </FormDialog>
      )}

      {secret && (
        <ConnectDialog
          secret={secret.key}
          allowedModels={secret.models}
          onClose={() => setSecret(null)}
        />
      )}
      {connecting && (
        <ConnectDialog
          allowedModels={connecting.allowed_models}
          onClose={() => setConnecting(null)}
        />
      )}

      <Confirm
        target={toggling}
        title={toggling?.disabled ? "Enable key" : "Disable key"}
        description={
          toggling?.disabled
            ? `Enable "${toggling?.name || toggling?.key_prefix}"? Clients holding it regain access immediately.`
            : `Disable "${toggling?.name || toggling?.key_prefix}"? Requests using it start failing immediately.`
        }
        actionLabel={toggling?.disabled ? "Enable" : "Disable"}
        isLoading={update.isPending}
        onCancel={() => setToggling(null)}
        onConfirm={async () => {
          if (!toggling) return;
          await toggleDisabled(toggling);
          setToggling(null);
        }}
      />

      <Confirm
        target={deleting}
        title="Delete key"
        description={`Delete "${deleting?.name || deleting?.key_prefix}"? Clients using it will immediately lose access.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Key deleted" });
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
