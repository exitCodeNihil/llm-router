import { useMemo, useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Avatar } from "@astryxdesign/core/Avatar";
import { Divider } from "@astryxdesign/core/Divider";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { Spinner } from "@astryxdesign/core/Spinner";
import { useToast } from "@astryxdesign/core/Toast";

import type { TeamMember, Me, Team } from "../lib/types";
import {
  useAddMember,
  useCreateTeam,
  useDeleteTeam,
  useRemoveMember,
  useTeamMembers,
  useUpdateMember,
  useTeams,
  useUpdateTeam,
  useUsers,
} from "../lib/hooks";
import { budgetLabel } from "../lib/format";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import {
  ModelPolicyField,
  ModelsCell,
  LimitsFields,
  TagChips,
  TagsInput,
  emptyLimits,
  limitsFrom,
  limitsPayload,
  type LimitsState,
} from "../components/forms";
import { IconEdit, IconPlus, IconTeams, IconTrash } from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";

function TeamForm({
  editing,
  onClose,
}: {
  editing: Team | null;
  onClose: () => void;
}) {
  const create = useCreateTeam();
  const update = useUpdateTeam();
  const showToast = useToast();

  const [name, setName] = useState(editing?.name ?? "");
  const [tags, setTags] = useState<string[]>(editing?.tags ?? []);
  const [models, setModels] = useState<string[]>(editing?.allowed_models ?? []);
  const [limits, setLimits] = useState<LimitsState>(
    editing ? limitsFrom(editing) : emptyLimits,
  );

  const busy = create.isPending || update.isPending;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    // null, not [], clears the policy: an empty list would allow nothing.
    const base = {
      name: name.trim(),
      tags,
      allowed_models: models.length ? models : null,
      ...limitsPayload(limits),
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...base });
        showToast({ body: "Team updated" });
      } else {
        await create.mutateAsync(base);
        showToast({ body: "Team created" });
      }
      onClose();
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          placeholder="Platform"
          isRequired
          hasAutoFocus
        />
        <ModelPolicyField value={models} onChange={setModels} subject="team" />
        <LimitsFields value={limits} onChange={setLimits} />
        <TagsInput
          value={tags}
          onChange={setTags}
          description="Group and scope by label, e.g. prod, debug"
        />
        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Create team"}
          isLoading={busy}
          isDisabled={!name.trim()}
        />
      </VStack>
    </form>
  );
}

function MemberBudgetForm({
  teamId,
  member,
  onClose,
}: {
  teamId: string;
  member: TeamMember;
  onClose: () => void;
}) {
  const update = useUpdateMember(teamId);
  const showToast = useToast();
  const [budget, setBudget] = useState(
    member.budget_usd == null ? "" : String(member.budget_usd),
  );
  const [period, setPeriod] = useState<string>(
    member.budget_period ?? "monthly",
  );
  const invalid = budget !== "" && Number.isNaN(Number(budget));

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await update.mutateAsync({
        uid: member.user_id,
        // Blank clears the share; the member is then capped only by the team
        // pool and their own user budget.
        budget_usd: budget === "" ? null : Number(budget),
        budget_period: budget === "" ? null : period,
      });
      showToast({ body: budget === "" ? "Share cleared" : "Share saved" });
      onClose();
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <HStack gap={3} vAlign="end" wrap="wrap">
          <TextInput
            label="Budget (USD)"
            placeholder="No share"
            value={budget}
            onChange={(v) => setBudget(v.replace(/[^0-9.]/g, ""))}
            description="Blank = no per-member cap; 0 blocks this member's team keys"
            status={
              invalid ? { type: "error", message: "Enter a number" } : undefined
            }
            hasAutoFocus
          />
          <Selector
            label="Period"
            value={period}
            onChange={setPeriod}
            isDisabled={budget === ""}
            options={[
              { value: "daily", label: "Daily" },
              { value: "monthly", label: "Monthly" },
              { value: "total", label: "Total" },
            ]}
          />
        </HStack>
        <FormActions
          onCancel={onClose}
          submitLabel="Save share"
          isLoading={update.isPending}
          isDisabled={invalid}
        />
      </VStack>
    </form>
  );
}

function MembersPanel({ team, me }: { team: Team; me: Me }) {
  const members = useTeamMembers(team.id);
  const add = useAddMember(team.id);
  const remove = useRemoveMember(team.id);
  const users = useUsers(me.is_admin);
  const showToast = useToast();

  const [userId, setUserId] = useState("");
  const [role, setRole] = useState("member");

  const canManage = me.is_admin || me.team_roles[team.id] === "admin";

  // Only offer users who are not already on the team.
  const available = useMemo(() => {
    const have = new Set((members.data ?? []).map((m) => m.user_id));
    return (users.data ?? []).filter((u) => !have.has(u.id));
  }, [users.data, members.data]);

  async function addMember(e: React.FormEvent) {
    e.preventDefault();
    if (!userId.trim()) return;
    try {
      await add.mutateAsync({ user_id: userId.trim(), role });
      showToast({ body: "Member added" });
      setUserId("");
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  const [budgeting, setBudgeting] = useState<TeamMember | null>(null);
  const [removing, setRemoving] = useState<{
    uid: string;
    label: string;
  } | null>(null);
  async function removeMember(uid: string, label: string) {
    try {
      await remove.mutateAsync(uid);
      showToast({ body: `Removed ${label}` });
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
    setRemoving(null);
  }

  return (
    <VStack gap={4}>
      <span className="eyebrow">Members</span>

      {members.isLoading ? (
        <HStack gap={2} vAlign="center">
          <Spinner size="sm" />
          <Text type="supporting" color="secondary">
            Loading members…
          </Text>
        </HStack>
      ) : (members.data?.length ?? 0) === 0 ? (
        <Text type="supporting" color="secondary">
          No members yet.
        </Text>
      ) : (
        <VStack gap={2}>
          {(members.data ?? []).map((m) => (
            <HStack key={m.user_id} gap={3} vAlign="center">
              <Avatar name={m.name || m.email} size="sm" />
              <VStack gap={0} width="100%">
                <Text type="body" maxLines={1}>
                  {m.name || m.email}
                </Text>
                {m.name && (
                  <Text type="supporting" color="secondary" maxLines={1}>
                    {m.email}
                  </Text>
                )}
              </VStack>
              <Badge
                variant={m.role === "admin" ? "purple" : "neutral"}
                label={m.role}
              />
              {canManage ? (
                <Button
                  label={
                    m.budget_usd == null
                      ? "No share"
                      : budgetLabel(m.budget_usd, m.budget_period)
                  }
                  variant="ghost"
                  size="sm"
                  onClick={() => setBudgeting(m)}
                />
              ) : (
                <Text type="supporting" color="secondary">
                  {m.budget_usd == null
                    ? ""
                    : budgetLabel(m.budget_usd, m.budget_period)}
                </Text>
              )}
              {canManage && (
                <IconButton
                  label={`Remove ${m.email}`}
                  variant="ghost"
                  size="sm"
                  icon={<Icon icon={IconTrash} size="sm" />}
                  onClick={() =>
                    setRemoving({ uid: m.user_id, label: m.email })
                  }
                />
              )}
            </HStack>
          ))}
        </VStack>
      )}

      {canManage && (
        <>
          <Divider />
          <form onSubmit={addMember}>
            <VStack gap={2}>
              <span className="eyebrow">Add member</span>
              <HStack gap={2} vAlign="end" wrap="wrap">
                {me.is_admin ? (
                  <Selector
                    label="User"
                    value={userId}
                    onChange={setUserId}
                    placeholder="Select a user…"
                    hasSearch={available.length > 8}
                    options={available.map((u) => ({
                      value: u.id,
                      label: u.email,
                    }))}
                  />
                ) : (
                  <TextInput
                    label="Email"
                    value={userId}
                    onChange={setUserId}
                    placeholder="person@company.com"
                    description="The address they sign in with"
                  />
                )}
                <Selector
                  label="Role"
                  value={role}
                  onChange={setRole}
                  options={[
                    { value: "member", label: "Member" },
                    { value: "admin", label: "Admin" },
                  ]}
                />
                <Button
                  label="Add"
                  type="submit"
                  variant="primary"
                  isLoading={add.isPending}
                  isDisabled={!userId.trim()}
                />
              </HStack>
            </VStack>
          </form>
        </>
      )}
      {budgeting && (
        <FormDialog
          isOpen
          onClose={() => setBudgeting(null)}
          title={`Budget share for ${budgeting.email}`}
          subtitle={`How much of ${team.name}'s budget this member may spend through the team's keys.`}
        >
          <MemberBudgetForm
            teamId={team.id}
            member={budgeting}
            onClose={() => setBudgeting(null)}
          />
        </FormDialog>
      )}
      <Confirm
        target={removing}
        title="Remove member"
        description={`Remove ${removing?.label} from ${team.name}? They lose the team budget and access to its keys.`}
        actionLabel="Remove"
        isLoading={remove.isPending}
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && removeMember(removing.uid, removing.label)}
      />
    </VStack>
  );
}

export default function Teams({ me }: { me: Me }) {
  const teams = useTeams();
  const del = useDeleteTeam();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<Team | null>(null);
  const [detail, setDetail] = useState<Team | null>(null);
  const [deleting, setDeleting] = useState<Team | null>(null);

  const columns: TableColumn<Team & Record<string, unknown>>[] = [
    {
      key: "name",
      header: "Team",
      sortable: true,
      width: proportional(1.4),
      renderCell: (t) => (
        <HStack gap={2} vAlign="center">
          <Icon icon={IconTeams} size="sm" color="secondary" />
          <Text type="body" weight="medium" maxLines={1}>
            {t.name}
          </Text>
          {me.team_roles[t.id] === "admin" && (
            <Badge variant="purple" label="you admin" />
          )}
        </HStack>
      ),
    },
    {
      key: "allowed_models",
      header: "Models",
      width: pixel(96),
      renderCell: (t) => <ModelsCell models={t.allowed_models} />,
    },
    {
      key: "tags",
      header: "Tags",
      width: proportional(1),
      renderCell: (t) => <TagChips tags={t.tags} />,
    },
    {
      key: "budget_usd",
      header: "Budget",
      width: pixel(130),
      renderCell: (t) => budgetLabel(t.budget_usd, t.budget_period),
    },
    {
      key: "rpm_limit",
      header: "Limits",
      width: pixel(140),
      renderCell: (t) => (
        <Text type="supporting" color="secondary">
          {t.rpm_limit ? `${t.rpm_limit} rpm` : "—"}
          {t.tpm_limit ? ` · ${t.tpm_limit} tpm` : ""}
        </Text>
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(150),
      align: "end",
      renderCell: (t) => (
        <HStack gap={1} hAlign="end">
          <Button
            label="Members"
            variant="ghost"
            size="sm"
            onClick={() => setDetail(t)}
          />
          {me.is_admin && (
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
          )}
          {me.is_admin && (
            <IconButton
              label={`Delete ${t.name}`}
              variant="ghost"
              size="sm"
              icon={<Icon icon={IconTrash} size="sm" />}
              onClick={() => setDeleting(t)}
            />
          )}
        </HStack>
      ),
    },
  ];

  const tools = useTableTools(
    teams.data as Array<Team & Record<string, unknown>> | undefined,
    {
      search: (t) => `${t.name} ${(t.tags ?? []).join(" ")}`,
      defaultSort: [{ sortKey: "name", direction: "ascending" }],
    },
  );

  const addButton = me.is_admin ? (
    <Button
      label="New team"
      variant="primary"
      icon={<Icon icon={IconPlus} size="sm" />}
      onClick={() => {
        setEditing(null);
        setShowForm(true);
      }}
    />
  ) : undefined;

  return (
    <Page
      eyebrow="Access"
      title="Teams"
      description="Group users, share budgets and rate limits, and delegate key management to team admins."
      action={
        <HStack gap={2} vAlign="center">
          <SearchBox
            value={tools.query}
            onChange={tools.setQuery}
            placeholder="Search teams…"
          />
          {addButton}
        </HStack>
      }
    >
      <Card padding={0}>
        {teams.isLoading ? (
          <Loading />
        ) : teams.error ? (
          <ErrorState error={teams.error} onRetry={() => teams.refetch()} />
        ) : (teams.data?.length ?? 0) === 0 ? (
          <Empty
            title="No teams yet"
            description="Create a team to pool budget across users."
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
          title={editing ? "Edit team" : "Create team"}
          wide
        >
          <TeamForm editing={editing} onClose={() => setShowForm(false)} />
        </FormDialog>
      )}

      {detail && (
        <FormDialog
          isOpen
          onClose={() => setDetail(null)}
          title={detail.name}
          subtitle="Team members"
          wide
        >
          <Grid columns={{ minWidth: 260, repeat: "fit" }} gap={4}>
            <MembersPanel team={detail} me={me} />
          </Grid>
        </FormDialog>
      )}

      <Confirm
        target={deleting}
        title="Delete team"
        description={`Delete "${deleting?.name}"? Every API key owned by this team is revoked immediately and its members lose the shared budget.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            // The dialog above is the confirmation the API asks for.
            await del.mutateAsync({ id: deleting.id, revokeKeys: true });
            showToast({ body: "Team deleted" });
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
