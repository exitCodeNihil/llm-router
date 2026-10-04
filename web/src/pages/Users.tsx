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
import { Switch } from "@astryxdesign/core/Switch";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { Timestamp } from "@astryxdesign/core/Timestamp";
import { useToast } from "@astryxdesign/core/Toast";

import type { User } from "../lib/types";
import {
  useCreateUser,
  useDeleteUser,
  useSetUserPassword,
  useUpdateUser,
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
import { IconEdit, IconPlus, IconTrash } from "../components/icons";
import { SearchBox, useTableTools } from "../components/tables";

function UserForm({
  editing,
  onClose,
}: {
  editing: User | null;
  onClose: () => void;
}) {
  const create = useCreateUser();
  const update = useUpdateUser();
  const setPassword = useSetUserPassword();
  const showToast = useToast();

  const [email, setEmail] = useState(editing?.email ?? "");
  const [password, setPasswordValue] = useState("");
  const [name, setName] = useState(editing?.name ?? "");
  const [role, setRole] = useState(editing?.role ?? "member");
  const [tags, setTags] = useState<string[]>(editing?.tags ?? []);
  const [models, setModels] = useState<string[]>(editing?.allowed_models ?? []);
  const [limits, setLimits] = useState<LimitsState>(
    editing ? limitsFrom(editing) : emptyLimits,
  );

  const busy = create.isPending || update.isPending || setPassword.isPending;
  const passwordShort = password.length > 0 && password.length < 8;
  // Local-only domains (dev@local) are valid here: the gateway is self-hosted.
  const emailValid = /^[^@\s]+@[^@\s]+$/.test(email.trim());

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    // The PATCH endpoint whitelists updatable columns and rejects the whole
    // request if it sees anything else — email is create-only, so it must not
    // be sent on edit.
    // null, not [], clears the policy: an empty list would allow nothing.
    const base = {
      name: name.trim(),
      role,
      tags,
      allowed_models: models.length ? models : null,
      ...limitsPayload(limits),
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...base });
        if (password)
          await setPassword.mutateAsync({ id: editing.id, password });
        showToast({
          body: password ? "User updated and password set" : "User updated",
        });
      } else {
        await create.mutateAsync({
          ...base,
          email: email.trim(),
          ...(password ? { password } : {}),
        });
        showToast({ body: "User created" });
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
          label="Email"
          type="email"
          value={email}
          onChange={setEmail}
          placeholder="person@company.com"
          isRequired
          hasAutoFocus={!editing}
          isDisabled={!!editing}
          disabledMessage="Email identifies the user and cannot be changed after creation"
          status={
            email && !emailValid
              ? { type: "error", message: "Enter a valid email" }
              : undefined
          }
        />
        <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
          <TextInput
            label="Name"
            value={name}
            onChange={setName}
            placeholder="Ada Lovelace"
            isOptional
          />
          <Selector
            label="Role"
            value={role}
            onChange={setRole}
            options={[
              { value: "member", label: "Member" },
              { value: "admin", label: "Admin" },
            ]}
          />
        </Grid>
        <TextInput
          label={editing ? "New password" : "Password"}
          type="password"
          value={password}
          onChange={setPasswordValue}
          placeholder={
            editing
              ? "Leave blank to keep the current one"
              : "At least 8 characters"
          }
          description={
            editing
              ? "Sets a new password for this user; they are not signed out."
              : "Leave blank if they will sign in with SSO. An admin can set one later from Edit."
          }
          status={
            passwordShort
              ? { type: "error", message: "At least 8 characters" }
              : undefined
          }
          isOptional
        />
        <ModelPolicyField value={models} onChange={setModels} subject="user" />
        <LimitsFields value={limits} onChange={setLimits} />
        <TagsInput
          value={tags}
          onChange={setTags}
          description="Group and scope by label, e.g. prod, debug"
        />
        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Create user"}
          isLoading={busy}
          isDisabled={(!editing && !emailValid) || passwordShort}
        />
      </VStack>
    </form>
  );
}

export default function Users() {
  const users = useUsers();
  const update = useUpdateUser();
  const del = useDeleteUser();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const [deleting, setDeleting] = useState<User | null>(null);

  // Confirmed rather than applied on click: disabling locks someone out of the
  // console and kills their keys; enabling hands all of it back.
  const [toggling, setToggling] = useState<User | null>(null);

  async function toggle(u: User) {
    try {
      await update.mutateAsync({ id: u.id, disabled: !u.disabled });
      showToast({ body: u.disabled ? "User enabled" : "User disabled" });
    } catch (err) {
      showToast({
        body: err instanceof Error ? err.message : "Failed",
        type: "error",
      });
    }
  }

  const columns: TableColumn<User & Record<string, unknown>>[] = [
    {
      key: "email",
      header: "User",
      sortable: true,
      width: proportional(1.5),
      renderCell: (u) => (
        <VStack gap={0}>
          <Text type="body" weight="medium" maxLines={1}>
            {u.email}
          </Text>
          {u.name && (
            <Text type="supporting" color="secondary" maxLines={1}>
              {u.name}
            </Text>
          )}
        </VStack>
      ),
    },
    {
      key: "role",
      header: "Role",
      sortable: true,
      width: pixel(96),
      renderCell: (u) => (
        <Badge
          variant={u.role === "admin" ? "purple" : "neutral"}
          label={u.role}
        />
      ),
    },
    {
      key: "allowed_models",
      header: "Models",
      width: pixel(96),
      renderCell: (u) => <ModelsCell models={u.allowed_models} />,
    },
    {
      key: "tags",
      header: "Tags",
      width: proportional(1),
      renderCell: (u) => <TagChips tags={u.tags} />,
    },
    {
      key: "budget_usd",
      header: "Budget",
      width: pixel(124),
      renderCell: (u) => budgetLabel(u.budget_usd, u.budget_period),
    },
    {
      key: "created_at",
      header: "Added",
      sortable: true,
      width: pixel(110),
      renderCell: (u) => <Timestamp value={u.created_at} format="date" />,
    },
    {
      key: "disabled",
      header: "Active",
      width: pixel(84),
      renderCell: (u) => (
        <Switch
          label={`${u.disabled ? "Enable" : "Disable"} ${u.email}`}
          isLabelHidden
          value={!u.disabled}
          changeAction={() => setToggling(u)}
        />
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(88),
      align: "end",
      renderCell: (u) => (
        <HStack gap={1} hAlign="end">
          <IconButton
            label={`Edit ${u.email}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconEdit} size="sm" />}
            onClick={() => {
              setEditing(u);
              setShowForm(true);
            }}
          />
          <IconButton
            label={`Delete ${u.email}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={() => setDeleting(u)}
          />
        </HStack>
      ),
    },
  ];

  const tools = useTableTools(
    users.data as Array<User & Record<string, unknown>> | undefined,
    {
      search: (u) => `${u.email} ${u.role} ${(u.tags ?? []).join(" ")}`,
      defaultSort: [{ sortKey: "email", direction: "ascending" }],
    },
  );

  const addButton = (
    <Button
      label="New user"
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
      eyebrow="Operations"
      title="Users"
      description="People who can hold keys and spend budget. SSO can create these automatically."
      action={
        <HStack gap={2} vAlign="center">
          <SearchBox
            value={tools.query}
            onChange={tools.setQuery}
            placeholder="Search users…"
          />
          {addButton}
        </HStack>
      }
    >
      <Card padding={0}>
        {users.isLoading ? (
          <Loading />
        ) : users.error ? (
          <ErrorState error={users.error} onRetry={() => users.refetch()} />
        ) : (users.data?.length ?? 0) === 0 ? (
          <Empty
            title="No users yet"
            description="Add a user, or enable SSO auto-provisioning."
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
          title={editing ? "Edit user" : "Create user"}
          wide
        >
          <UserForm editing={editing} onClose={() => setShowForm(false)} />
        </FormDialog>
      )}

      <Confirm
        target={toggling}
        title={toggling?.disabled ? "Enable user" : "Disable user"}
        description={
          toggling?.disabled
            ? `Enable ${toggling?.email}? They regain console access and their keys start working again.`
            : `Disable ${toggling?.email}? They lose console access and their keys stop working immediately.`
        }
        actionLabel={toggling?.disabled ? "Enable" : "Disable"}
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
        title="Delete user"
        description={`Delete ${deleting?.email}? Their keys stop working immediately.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "User deleted" });
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
