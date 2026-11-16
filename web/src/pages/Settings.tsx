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
import { Banner } from "@astryxdesign/core/Banner";
import { Code } from "@astryxdesign/core/Code";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
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
import { Empty, ErrorState, Loading, Page, SettingsSection } from "../components/Page";
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
  const secretError = sso.data?.secret_error;

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

  return (
    <SettingsSection
      title="Console sign-in"
      description="Single sign-on through your OIDC provider, for people opening this console in a browser. It does not affect API requests."
      aside={
        sso.data &&
        (configured ? (
          <Badge variant="success" label="configured" />
        ) : (
          <Badge variant="warning" label="not configured" />
        ))
      }
    >
      {sso.isLoading ? (
        <Loading />
      ) : sso.error ? (
        <ErrorState error={sso.error} onRetry={() => sso.refetch()} />
      ) : (
        <Card padding={4}>
          <form onSubmit={submit} noValidate>
            <VStack gap={4}>
              <TextInput
                label="Issuer URL"
                value={issuer}
                onChange={setIssuer}
                placeholder="https://login.microsoftonline.com/…/v2.0"
                description="Your provider's OIDC discovery base URL"
                isRequired
              />
              <TextInput label="Client ID" value={clientId} onChange={setClientId} isRequired />
              {secretError && (
                <Banner status="error" title="Sign-in with SSO is not working" description={secretError} />
              )}
              <TextInput
                label="Client secret"
                type="password"
                value={clientSecret}
                onChange={setClientSecret}
                placeholder={configured && !secretError ? "••••••••" : "Paste the client secret"}
                description={
                  configured && !secretError ? "A secret is stored, encrypted. Leave blank to keep it." : undefined
                }
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

              <Divider />
              <HStack hAlign="end">
                <Button
                  label="Save changes"
                  type="submit"
                  variant="primary"
                  isLoading={save.isPending}
                  isDisabled={!issuer.trim() || !clientId.trim() || !redirect.trim()}
                />
              </HStack>
            </VStack>
          </form>
        </Card>
      )}
    </SettingsSection>
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
            description="The token's aud claim must equal this"
            isRequired
          />
        </Grid>

        <TextInput
          label="Issuer URL"
          value={issuerUrl}
          onChange={setIssuerUrl}
          placeholder={ISSUER_HINT[type]}
          description="Must match the token's iss claim exactly"
          isRequired
        />

        <Divider />
        <VStack gap={0}>
          <Heading level={5}>Who the caller is</Heading>
          <Text type="supporting" color="secondary">
            How a token is matched to a user of this router.
          </Text>
        </VStack>

        <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={3}>
          <TextInput
            label="Email claim"
            value={emailClaim}
            onChange={setEmailClaim}
            placeholder="email"
            description="Claim that names the caller. Blank uses email."
            isOptional
          />
          <Selector
            label="Map to team"
            value={mapToTeam}
            // hasClear widens onChange to `string | null`; normalise back to "".
            onChange={(v) => setMapToTeam(v ?? "")}
            placeholder="No team"
            hasClear
            description="New users from this issuer join this team"
            options={(teams.data ?? []).map((t) => ({ value: t.id, label: t.name }))}
          />
        </Grid>

        <Switch
          label="Auto-create user"
          description="Create a user the first time a new caller presents a token. When off, only existing users are accepted."
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

// ── How to use cloud identity tokens ────────────────────────────
type IssuerType = TokenIssuer["type"];

const ISSUER_SETUP: Record<
  IssuerType,
  {
    label: string;
    issuer: string;
    audience: string;
    claim: string;
    note: string;
  }
> = {
  entra: {
    label: "Azure Entra ID",
    issuer: "https://login.microsoftonline.com/<tenant>/v2.0",
    audience: "api://llm-router",
    claim: "preferred_username",
    note: "In the app registration manifest, set accessTokenAcceptedVersion to 2. Otherwise the token's issuer does not match.",
  },
  gcp: {
    label: "Google Cloud",
    issuer: "https://accounts.google.com",
    audience: "https://router.example.com",
    claim: "email",
    note: "The audience can be any value. Callers must ask for a token with the same one.",
  },
  oidc: {
    label: "Other OIDC",
    issuer: "https://idp.example.com",
    audience: "llm-router",
    claim: "email",
    note: "For machine tokens that carry no email, set Email claim to a stable claim such as client_id.",
  },
};

function tokenCommand(type: IssuerType, audience: string): string {
  if (type === "entra")
    return `TOKEN=$(az account get-access-token \\\n  --resource ${audience} --query accessToken -o tsv)`;
  if (type === "gcp") return `TOKEN=$(gcloud auth print-identity-token --audiences=${audience})`;
  return `TOKEN=<a JWT from your issuer with aud "${audience}">`;
}

function IssuerGuide({ issuers, onClose }: { issuers: TokenIssuer[]; onClose: () => void }) {
  const [type, setType] = useState<IssuerType>(issuers[0]?.type ?? "entra");
  const setup = ISSUER_SETUP[type];
  // An issuer already added gives the real audience to request.
  const audience = issuers.find((i) => i.type === type)?.audience || setup.audience;
  const code = [
    tokenCommand(type, audience),
    ``,
    `curl ${window.location.origin}/v1/chat/completions \\`,
    `  -H "Authorization: Bearer $TOKEN" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d '{"model":"my-model","messages":[{"role":"user","content":"hi"}]}'`,
  ].join("\n");

  const steps = [
    "Add an issuer here: the identity provider that signs the tokens, and the audience they are issued for.",
    "The caller gets a token from its platform and sends it as Authorization: Bearer <token>, where an API key would go.",
    "The gateway checks the signature against the issuer's published keys, then the audience and the expiry. No secret is stored.",
    "The caller is matched to a user by the email claim. That user's teams, model access, limits and budgets apply, and usage is recorded under them.",
  ];

  return (
    <Dialog
      isOpen
      onOpenChange={(open) => !open && onClose()}
      purpose="info"
      width={680}
      maxHeight="calc(100vh - 48px)"
    >
      <DialogHeader
        title="Using cloud identity tokens"
        subtitle="Workloads on Azure, Google Cloud or another OIDC issuer call the API with the identity they already have, so there is no long-lived key to hand out or rotate."
      />
      <VStack gap={5} padding={4} isScrollable style={{ minHeight: 0 }}>
        <VStack gap={3}>
          <Heading level={5}>How it works</Heading>
          <ol className="prose-list">
            {steps.map((step) => (
              <li key={step}>{step}</li>
            ))}
          </ol>
        </VStack>

        <Divider />
        <VStack gap={3}>
          <Heading level={5}>Set up and call</Heading>
          <SegmentedControl label="Issuer type" size="sm" value={type} onChange={(v) => setType(v as IssuerType)}>
            {(Object.keys(ISSUER_SETUP) as IssuerType[]).map((t) => (
              <SegmentedControlItem key={t} value={t} label={ISSUER_SETUP[t].label} />
            ))}
          </SegmentedControl>
          <dl className="kv">
            <dt>Issuer URL</dt>
            <dd>{setup.issuer}</dd>
            <dt>Audience</dt>
            <dd>{audience}</dd>
            <dt>Email claim</dt>
            <dd>{setup.claim}</dd>
          </dl>
          <Text type="supporting" color="secondary">
            {setup.note}
          </Text>
          {/* The scrolling column would otherwise squeeze the block to nothing. */}
          <div style={{ flexShrink: 0 }}>
            <CodeBlock code={code} language="bash" title="shell" width="100%" maxHeight={240} />
          </div>
        </VStack>

        <Divider />
        <VStack gap={3}>
          <Heading level={5}>Good to know</Heading>
          <ul className="prose-list">
            <li>
              A caller with no matching user is refused unless Auto-create user is on. Map to team puts new users in a
              team, so they get that team's models and budgets.
            </li>
            <li>
              Enabling an issuer accepts every token it signs for the audience. Use an audience that is specific to this
              router, not one shared with other services.
            </li>
            <li>
              The issuer URL must be https, and the control plane and every gateway must be able to reach it to fetch
              its keys.
            </li>
            <li>
              This is separate from console sign-in, which signs people in to this console. The same identity provider
              can be used for both.
            </li>
          </ul>
        </VStack>

        <HStack gap={2} hAlign="end">
          <Button label="Done" variant="primary" onClick={onClose} />
        </HStack>
      </VStack>
    </Dialog>
  );
}

function TokenIssuers() {
  const issuers = useTokenIssuers();
  const update = useUpdateTokenIssuer();
  const del = useDeleteTokenIssuer();
  const showToast = useToast();

  const [showForm, setShowForm] = useState(false);
  const [showGuide, setShowGuide] = useState(false);
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

  const count = issuers.data?.length ?? 0;

  return (
    <SettingsSection
      title="API identity tokens"
      description="Cloud token issuers let apps and workloads call the API with an identity token from Azure, Google Cloud or another OIDC issuer instead of an API key."
      aside={<Button label="How to use" variant="secondary" size="sm" onClick={() => setShowGuide(true)} />}
    >
      <Card padding={0}>
        <VStack gap={0}>
          {count > 0 && (
            <>
              <HStack padding={3} hAlign="between" vAlign="center" gap={3}>
                <Text type="supporting" color="secondary">
                  {count} {count === 1 ? "issuer" : "issuers"}
                </Text>
                {addButton}
              </HStack>
              <Divider />
            </>
          )}
          {issuers.isLoading ? (
            <Loading />
          ) : issuers.error ? (
            <ErrorState error={issuers.error} onRetry={() => issuers.refetch()} />
          ) : count === 0 ? (
            <Empty
              title="No token issuers"
              description="API requests are accepted with API keys only. Add an issuer to also accept identity tokens from a cloud platform."
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

        {showGuide && <IssuerGuide issuers={issuers.data ?? []} onClose={() => setShowGuide(false)} />}

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
    </SettingsSection>
  );
}

export default function Settings() {
  return (
    <Page
      eyebrow="Settings"
      title="Settings"
      description="How people sign in to the console, and how workloads authenticate to the API without a key. Workspaces have their own page under Develop."
    >
      <VStack gap={6} maxWidth={1080}>
        <Divider />
        <SsoCard />
        <Divider />
        <TokenIssuers />
      </VStack>
    </Page>
  );
}
