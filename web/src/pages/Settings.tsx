import { useEffect, useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Code } from "@astryxdesign/core/Code";
import { Switch } from "@astryxdesign/core/Switch";
import { Divider } from "@astryxdesign/core/Divider";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { Table, pixel, proportional } from "@astryxdesign/core/Table";
import type { TableColumn } from "@astryxdesign/core/Table";
import { useToast } from "@astryxdesign/core/Toast";

import type { TokenIssuer } from "../lib/types";
import {
  useCreateTokenIssuer,
  useDeleteTokenIssuer,
  useSetSso,
  useSso,
  useTeams,
  useTokenIssuers,
  useUpdateTokenIssuer,
} from "../lib/hooks";
import { Empty, ErrorState, Loading, Page } from "../components/Page";
import { Confirm, FormActions, FormDialog } from "../components/dialogs";
import { IconEdit, IconPlus, IconTrash } from "../components/icons";

const ISSUER_HINT: Record<string, string> = {
  entra: "https://login.microsoftonline.com/<tenant>/v2.0",
  gcp: "https://accounts.google.com",
  oidc: "Your provider's OIDC discovery base URL",
};

// ── SSO ─────────────────────────────────────────────────────────
function SsoCard() {
  const sso = useSso();
  const save = useSetSso();
  const showToast = useToast();

  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [redirect, setRedirect] = useState("");
  const [autoCreate, setAutoCreate] = useState(false);
  const configured = sso.data?.configured;

  // Hydrate once settings load. client_secret always comes back empty.
  useEffect(() => {
    const s = sso.data?.sso;
    if (!s) return;
    setIssuer(s.issuer_url);
    setClientId(s.client_id);
    setRedirect(s.redirect_url);
    setAutoCreate(s.auto_create_users);
  }, [sso.data]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await save.mutateAsync({
        issuer_url: issuer.trim(),
        client_id: clientId.trim(),
        client_secret: clientSecret, // empty keeps the stored secret
        redirect_url: redirect.trim(),
        auto_create_users: autoCreate,
      });
      setClientSecret("");
      showToast({ body: "SSO settings saved" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  if (sso.isLoading) return <Loading />;
  if (sso.error) return <ErrorState error={sso.error} onRetry={() => sso.refetch()} />;

  return (
    <Card>
      <form onSubmit={submit} noValidate>
        <VStack gap={4}>
          <HStack hAlign="between" vAlign="center">
            <Heading level={4}>OIDC provider</Heading>
            {configured ? (
              <Badge variant="success" label="configured" />
            ) : (
              <Badge variant="warning" label="not configured" />
            )}
          </HStack>

          <TextInput
            label="Issuer URL"
            value={issuer}
            onChange={setIssuer}
            placeholder="https://login.microsoftonline.com/…/v2.0"
            description="Your provider's OIDC discovery base URL"
            isRequired
          />
          <TextInput label="Client ID" value={clientId} onChange={setClientId} isRequired />
          <TextInput
            label="Client secret"
            type="password"
            value={clientSecret}
            onChange={setClientSecret}
            placeholder={configured ? "••••••••" : "Paste the client secret"}
            description={configured ? "A secret is stored. Leave blank to keep it." : undefined}
          />
          <TextInput
            label="Redirect URL"
            value={redirect}
            onChange={setRedirect}
            placeholder="https://gateway.example.com/auth/callback"
            description="Must match the callback registered with your provider"
            isRequired
          />

          <Switch
            label="Auto-create users"
            description="Provision a user on first successful sign-in"
            value={autoCreate}
            onChange={setAutoCreate}
          />

          <HStack hAlign="end">
            <Button
              label="Save SSO settings"
              type="submit"
              variant="primary"
              isLoading={save.isPending}
              isDisabled={!issuer.trim() || !clientId.trim() || !redirect.trim()}
            />
          </HStack>
        </VStack>
      </form>
    </Card>
  );
}

// ── Cloud token issuers ─────────────────────────────────────────
function IssuerForm({ editing, onClose }: { editing: TokenIssuer | null; onClose: () => void }) {
  const create = useCreateTokenIssuer();
  const update = useUpdateTokenIssuer();
  const teams = useTeams();
  const showToast = useToast();

  const cm = editing?.claim_mapping ?? {};
  const [type, setType] = useState<TokenIssuer["type"]>(editing?.type ?? "entra");
  const [issuerUrl, setIssuerUrl] = useState(editing?.issuer_url ?? "");
  const [audience, setAudience] = useState(editing?.audience ?? "");
  const [emailClaim, setEmailClaim] = useState(cm.email_claim ?? "");
  const [mapToTeam, setMapToTeam] = useState(cm.map_to_team ?? "");
  const [autoCreate, setAutoCreate] = useState(cm.auto_create_user ?? false);

  const busy = create.isPending || update.isPending;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    // `type` is create-only: the PATCH allow-list rejects it.
    const body = {
      ...(editing ? {} : { type }),
      issuer_url: issuerUrl.trim(),
      audience: audience.trim(),
      claim_mapping: {
        ...(emailClaim ? { email_claim: emailClaim.trim() } : {}),
        ...(mapToTeam ? { map_to_team: mapToTeam } : {}),
        auto_create_user: autoCreate,
      },
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, ...body });
        showToast({ body: "Issuer updated" });
      } else {
        await create.mutateAsync(body);
        showToast({ body: "Issuer added" });
      }
      onClose();
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <form onSubmit={submit} noValidate>
      <VStack gap={4}>
        <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
          <Selector
            label="Type"
            value={type}
            onChange={(v) => setType(v as TokenIssuer["type"])}
            isDisabled={!!editing}
            options={[
              { value: "entra", label: "Azure Entra ID" },
              { value: "gcp", label: "Google Cloud" },
              { value: "oidc", label: "Generic OIDC" },
            ]}
          />
          <TextInput
            label="Audience"
            value={audience}
            onChange={setAudience}
            placeholder="api://llm-router"
            description="Expected aud claim"
            isRequired
          />
        </Grid>

        <TextInput
          label="Issuer URL"
          value={issuerUrl}
          onChange={setIssuerUrl}
          placeholder={ISSUER_HINT[type]}
          description={ISSUER_HINT[type]}
          isRequired
        />

        <Divider />
        <span className="eyebrow">Claim mapping</span>

        <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
          <TextInput
            label="Email claim"
            value={emailClaim}
            onChange={setEmailClaim}
            placeholder="preferred_username"
            isOptional
          />
          <Selector
            label="Map to team"
            value={mapToTeam}
            // hasClear widens onChange to `string | null`; normalise back to "".
            onChange={(v) => setMapToTeam(v ?? "")}
            placeholder="No team"
            hasClear
            description="Membership granted on sign-in"
            options={(teams.data ?? []).map((t) => ({ value: t.id, label: t.name }))}
          />
        </Grid>

        <Switch
          label="Auto-create user"
          description="Provision a user the first time this issuer presents a new identity"
          value={autoCreate}
          onChange={setAutoCreate}
        />

        <FormActions
          onCancel={onClose}
          submitLabel={editing ? "Save changes" : "Add issuer"}
          isLoading={busy}
          isDisabled={!issuerUrl.trim() || !audience.trim()}
        />
      </VStack>
    </form>
  );
}

function TokenIssuers() {
  const issuers = useTokenIssuers();
  const update = useUpdateTokenIssuer();
  const del = useDeleteTokenIssuer();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<TokenIssuer | null>(null);
  const [deleting, setDeleting] = useState<TokenIssuer | null>(null);

  // Confirmed rather than applied on click: enabling an issuer means the gateway
  // starts accepting bearer tokens minted by it, which is an access grant.
  const [toggling, setToggling] = useState<TokenIssuer | null>(null);

  async function toggle(i: TokenIssuer) {
    try {
      await update.mutateAsync({ id: i.id, enabled: !i.enabled });
      showToast({ body: i.enabled ? "Issuer disabled" : "Issuer enabled" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  const columns: TableColumn<TokenIssuer & Record<string, unknown>>[] = [
    {
      key: "type",
      header: "Type",
      width: pixel(110),
      renderCell: (i) => (
        <Badge variant={i.type === "entra" ? "blue" : i.type === "gcp" ? "green" : "neutral"} label={i.type} />
      ),
    },
    {
      key: "issuer_url",
      header: "Issuer",
      width: proportional(2),
      renderCell: (i) => <Code>{i.issuer_url}</Code>,
    },
    {
      key: "audience",
      header: "Audience",
      width: proportional(1),
      renderCell: (i) => <Code>{i.audience}</Code>,
    },
    {
      key: "enabled",
      header: "Enabled",
      width: pixel(88),
      renderCell: (i) => (
        <Switch
          label={`${i.enabled ? "Disable" : "Enable"} ${i.issuer_url}`}
          isLabelHidden
          value={i.enabled}
          changeAction={() => setToggling(i)}
        />
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(88),
      align: "end",
      renderCell: (i) => (
        <HStack gap={1} hAlign="end">
          <IconButton
            label="Edit issuer"
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconEdit} size="sm" />}
            onClick={() => {
              setEditing(i);
              setShowForm(true);
            }}
          />
          <IconButton
            label="Delete issuer"
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={() => setDeleting(i)}
          />
        </HStack>
      ),
    },
  ];

  const addButton = (
    <Button
      label="Add issuer"
      variant="secondary"
      icon={<Icon icon={IconPlus} size="sm" />}
      onClick={() => {
        setEditing(null);
        setShowForm(true);
      }}
    />
  );

  return (
    <Card padding={0}>
      <VStack gap={0}>
        <HStack padding={4} hAlign="between" vAlign="center" gap={3} wrap="wrap">
          <VStack gap={0}>
            <Heading level={4}>Cloud token issuers</Heading>
            <Text type="supporting" color="secondary">
              Let callers authenticate with an Azure or GCP identity token instead of a gateway key.
            </Text>
          </VStack>
          {addButton}
        </HStack>
        <Divider />
        {issuers.isLoading ? (
          <Loading />
        ) : issuers.error ? (
          <ErrorState error={issuers.error} onRetry={() => issuers.refetch()} />
        ) : (issuers.data?.length ?? 0) === 0 ? (
          <Empty
            title="No token issuers"
            description="Add an issuer to accept platform identity tokens."
            action={addButton}
          />
        ) : (
          <Table
            data={(issuers.data ?? []) as Array<TokenIssuer & Record<string, unknown>>}
            columns={columns}
            idKey="id"
            density="balanced"
            dividers="rows"
            hasHover
            textOverflow="truncate"
          />
        )}
      </VStack>

      {showForm && (
        <FormDialog
          isOpen
          onClose={() => setShowForm(false)}
          title={editing ? "Edit token issuer" : "Add token issuer"}
          wide
        >
          <IssuerForm editing={editing} onClose={() => setShowForm(false)} />
        </FormDialog>
      )}

      <Confirm
        target={toggling}
        title={toggling?.enabled ? "Disable token issuer" : "Enable token issuer"}
        description={
          toggling?.enabled
            ? `Disable "${toggling?.issuer_url}"? Callers presenting its tokens are rejected immediately.`
            : `Enable "${toggling?.issuer_url}"? The gateway starts accepting any token this issuer mints for the configured audience.`
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
        title="Delete token issuer"
        description={`Delete the issuer "${deleting?.issuer_url}"? Callers using its tokens will be rejected.`}
        isLoading={del.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await del.mutateAsync(deleting.id);
            showToast({ body: "Issuer deleted" });
          } catch (err) {
            showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
          }
          setDeleting(null);
        }}
      />
    </Card>
  );
}

export default function Settings() {
  return (
    <Page
      eyebrow="Settings"
      title="Settings"
      description="How people sign in and which cloud identity tokens callers may present. Workspaces have their own page under Develop."
    >
      <VStack gap={4} maxWidth={760}>
        <SsoCard />
        <TokenIssuers />
      </VStack>
    </Page>
  );
}
