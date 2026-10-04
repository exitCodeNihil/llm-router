import { useEffect, useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { Card } from "@astryxdesign/core/Card";
import { Divider } from "@astryxdesign/core/Divider";
import { Grid } from "@astryxdesign/core/Grid";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Icon } from "@astryxdesign/core/Icon";
import { IconButton } from "@astryxdesign/core/IconButton";
import { NumberInput } from "@astryxdesign/core/NumberInput";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Selector } from "@astryxdesign/core/Selector";
import { Switch } from "@astryxdesign/core/Switch";
import { TextInput } from "@astryxdesign/core/TextInput";
import { useToast } from "@astryxdesign/core/Toast";

import type { WorkspaceScope } from "../../lib/types";
import { useSaveWorkspaceSettings, useTeams, useUsers, useWorkspaceSettings } from "../../lib/hooks";
import { ErrorState, Loading } from "../../components/Page";
import { IconPlus, IconTrash } from "../../components/icons";

export default function WorkspacesCard() {
  const settings = useWorkspaceSettings();
  const save = useSaveWorkspaceSettings();
  const teams = useTeams();
  const users = useUsers();
  const showToast = useToast();

  const [enabled, setEnabled] = useState(false);
  const [runtime, setRuntime] = useState<"docker" | "kubernetes">("docker");
  const [socket, setSocket] = useState("");
  const [storageClass, setStorageClass] = useState("");
  const [diskGB, setDiskGB] = useState<number | null>(5);
  const [gatewayURL, setGatewayURL] = useState("");
  const [image, setImage] = useState("");
  const [memoryMB, setMemoryMB] = useState<number | null>(2048);
  const [cpus, setCpus] = useState<number | null>(2);
  const [budget, setBudget] = useState<number | null>(5);
  const [maxPerUser, setMaxPerUser] = useState<number | null>(5);
  const [allow, setAllow] = useState<WorkspaceScope[]>([]);
  const [scopeType, setScopeType] = useState<"user" | "team">("user");
  const [scopeValue, setScopeValue] = useState("");

  // Hydrate once: a background refetch must not wipe half-entered edits.
  const hydrated = useRef(false);
  useEffect(() => {
    const s = settings.data;
    if (!s || hydrated.current) return;
    hydrated.current = true;
    setEnabled(s.enabled);
    setRuntime(s.runtime === "kubernetes" ? "kubernetes" : "docker");
    setSocket(s.socket ?? "");
    setStorageClass(s.storage_class ?? "");
    setDiskGB(s.disk_gb || null);
    setGatewayURL(s.gateway_url ?? "");
    setImage(s.default_image ?? "");
    setMemoryMB(s.memory_mb || null);
    setCpus(s.cpus || null);
    setBudget(s.budget_usd || null);
    setMaxPerUser(s.max_per_user || null);
    setAllow(s.allow ?? []);
  }, [settings.data]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await save.mutateAsync({
        enabled,
        runtime,
        socket: socket.trim(),
        storage_class: storageClass.trim(),
        disk_gb: diskGB ?? 0,
        gateway_url: gatewayURL.trim(),
        default_image: image.trim(),
        images: settings.data?.images ?? [],
        memory_mb: memoryMB ?? 0,
        cpus: cpus ?? 0,
        budget_usd: budget ?? 0,
        max_per_user: maxPerUser ?? 0,
        allow,
      });
      showToast({ body: "Workspace settings saved" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  function addScope() {
    if (!scopeValue) return;
    if (allow.some((a) => a.scope_type === scopeType && a.scope_value === scopeValue)) return;
    setAllow([...allow, { scope_type: scopeType, scope_value: scopeValue }]);
    setScopeValue("");
  }

  function labelFor(s: WorkspaceScope): string {
    if (s.scope_type === "team") {
      return teams.data?.find((t) => t.id === s.scope_value)?.name ?? s.scope_value;
    }
    return users.data?.find((u) => u.id === s.scope_value)?.email ?? s.scope_value;
  }

  if (settings.isLoading) return <Loading />;
  if (settings.error) return <ErrorState error={settings.error} onRetry={() => settings.refetch()} />;

  const options =
    scopeType === "team"
      ? (teams.data ?? []).map((t) => ({ value: t.id, label: t.name }))
      : (users.data ?? []).map((u) => ({ value: u.id, label: u.email }));

  return (
    <Card>
      <form onSubmit={submit} noValidate>
        <VStack gap={4}>
          <HStack hAlign="between" vAlign="center">
            <Heading level={4}>Runtime and limits</Heading>
            {enabled ? (
              <Badge variant="success" label="enabled" />
            ) : (
              <Badge variant="warning" label="disabled" />
            )}
          </HStack>

          <Text type="supporting" color="secondary">
            Workspaces run containers on this host. Everyone who can use them can execute
            arbitrary code in their own container, so the allow-list starts empty — meaning
            admins only.
          </Text>

          <Switch label="Enable workspaces" value={enabled} onChange={setEnabled} />

          <SegmentedControl
            label="Runtime"
            size="sm"
            value={runtime}
            onChange={(v) => setRuntime(v as "docker" | "kubernetes")}
          >
            <SegmentedControlItem value="docker" label="Docker / podman" />
            <SegmentedControlItem value="kubernetes" label="Kubernetes" />
          </SegmentedControl>

          {runtime === "docker" ? (
            <TextInput
              label="Container socket"
              value={socket}
              onChange={setSocket}
              placeholder="/run/user/1000/podman/podman.sock"
              description="Docker-compatible socket. On macOS this is podman machine's forwarding socket and changes across reboots."
              isRequired={enabled}
            />
          ) : (
            <>
              <Banner
                status="info"
                title="Cluster credentials come from the environment"
                description="LLMR_K8S_API plus either LLMR_K8S_TOKEN or LLMR_K8S_CLIENT_CERT/KEY. In-cluster they are detected automatically. They are never stored here; a client key is root on the cluster."
              />
              <Grid columns={{ minWidth: 160, repeat: "fit" }} gap={3}>
                <TextInput
                  label="Storage class"
                  value={storageClass}
                  onChange={setStorageClass}
                  placeholder="(cluster default)"
                  description="PVC storage class for workspace volumes"
                />
                <NumberInput
                  label="Disk (GB)"
                  value={diskGB}
                  onChange={setDiskGB}
                  min={1}
                  description="Size of each workspace PVC"
                />
              </Grid>
            </>
          )}
          <TextInput
            label="Gateway URL from inside a workspace"
            value={gatewayURL}
            onChange={setGatewayURL}
            placeholder="http://host.containers.internal:8080"
            description="How the agent reaches this gateway from its container — not the URL your browser uses."
          />
          <TextInput
            label="Default image"
            value={image}
            onChange={setImage}
            placeholder="llmr-workspace-base"
            description="Used when a workspace names no template (see Templates above)"
          />

          <Grid columns={{ minWidth: 160, repeat: "fit" }} gap={3}>
            <NumberInput label="Memory (MB)" value={memoryMB} onChange={setMemoryMB} min={0} />
            <NumberInput label="CPUs" value={cpus} onChange={setCpus} min={0} step={0.5} />
            <NumberInput
              label="Daily budget ($)"
              value={budget}
              onChange={setBudget}
              min={0}
              step={1}
              description="Spend cap on each workspace's agent key"
            />
            <NumberInput
              label="Workspaces per user"
              value={maxPerUser}
              onChange={setMaxPerUser}
              min={1}
              description="Each one is a container and a volume on this host"
            />
          </Grid>

          <Divider />

          <VStack gap={2}>
            <Text type="body" weight="medium">
              Who can use workspaces
            </Text>
            <Text type="supporting" color="secondary">
              Admins always can, which is why an empty list still works for you. Add users or teams to
              grant it to anyone else.
            </Text>

            {allow.map((s, i) => (
              <HStack key={`${s.scope_type}:${s.scope_value}`} gap={2} vAlign="center" hAlign="between">
                <HStack gap={2} vAlign="center">
                  <Badge variant="neutral" label={s.scope_type} />
                  <Text type="body">{labelFor(s)}</Text>
                </HStack>
                <IconButton
                  label="Remove"
                  variant="ghost"
                  size="sm"
                  icon={<Icon icon={IconTrash} size="sm" />}
                  onClick={() => setAllow(allow.filter((_, j) => j !== i))}
                />
              </HStack>
            ))}

            <HStack gap={2} vAlign="end">
              <Selector
                label="Scope"
                value={scopeType}
                onChange={(v) => {
                  setScopeType(v as "user" | "team");
                  setScopeValue("");
                }}
                options={[
                  { value: "user", label: "User" },
                  { value: "team", label: "Team" },
                ]}
              />
              <Selector
                label={scopeType === "team" ? "Team" : "User"}
                value={scopeValue}
                onChange={setScopeValue}
                placeholder="Select…"
                options={options}
              />
              <Button
                label="Add"
                variant="secondary"
                icon={<Icon icon={IconPlus} size="sm" />}
                isDisabled={!scopeValue}
                onClick={addScope}
              />
            </HStack>
          </VStack>

          <HStack hAlign="end">
            <Button label="Save" variant="primary" type="submit" isLoading={save.isPending} />
          </HStack>
        </VStack>
      </form>
    </Card>
  );
}
