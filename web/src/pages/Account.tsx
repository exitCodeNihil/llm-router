import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Badge } from "@astryxdesign/core/Badge";
import { Avatar } from "@astryxdesign/core/Avatar";
import { Divider } from "@astryxdesign/core/Divider";
import { TextInput } from "@astryxdesign/core/TextInput";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { useToast } from "@astryxdesign/core/Toast";

import type { Me } from "../lib/types";
import { api, token } from "../lib/api";
import { Page } from "../components/Page";
import { useMode } from "../mode";
import type { Mode } from "../theme";

export default function Account({ me }: { me: Me }) {
  const showToast = useToast();
  const { mode, toggle } = useMode();

  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);

  const mismatch = confirm.length > 0 && next !== confirm;
  const tooShort = next.length > 0 && next.length < 8;

  async function submitPassword(e: React.FormEvent) {
    e.preventDefault();
    if (next !== confirm) {
      showToast({ body: "Passwords do not match", type: "error" });
      return;
    }
    if (next.length < 8) {
      showToast({ body: "Password must be at least 8 characters", type: "error" });
      return;
    }
    setBusy(true);
    try {
      await api.post("/api/me/password", { current, new: next });
      showToast({ body: "Password updated" });
      setCurrent("");
      setNext("");
      setConfirm("");
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    } finally {
      setBusy(false);
    }
  }

  function signOut() {
    token.clear();
    window.location.href = "/auth/logout";
  }

  const teamCount = Object.keys(me.team_roles ?? {}).length;

  return (
    <Page eyebrow="Account" title="Account settings" description="Your profile, appearance, and password.">
      <VStack gap={4} maxWidth={640}>
        <Card>
          <VStack gap={4}>
            <HStack gap={3} vAlign="center">
              <Avatar name={me.email} size="lg" />
              <VStack gap={0}>
                <Heading level={4}>{me.email || "admin"}</Heading>
                <HStack gap={1.5} vAlign="center">
                  {me.is_admin && <Badge variant="purple" label="admin" />}
                  <Text type="supporting" color="secondary">
                    {teamCount} team{teamCount === 1 ? "" : "s"}
                  </Text>
                </HStack>
              </VStack>
            </HStack>
            <Divider />
            <MetadataList>
              <MetadataListItem label="User id">{me.user_id || "—"}</MetadataListItem>
              <MetadataListItem label="Email">{me.email || "—"}</MetadataListItem>
              <MetadataListItem label="Role">{me.is_admin ? "Administrator" : "Member"}</MetadataListItem>
            </MetadataList>
          </VStack>
        </Card>

        <Card>
          <VStack gap={3}>
            <Heading level={4}>Appearance</Heading>
            <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
              <Text type="supporting" color="secondary">
                Colour mode is stored locally and shared with the classic console.
              </Text>
              <SegmentedControl
                label="Colour mode"
                size="sm"
                value={mode}
                onChange={(v) => {
                  if ((v as Mode) !== mode) toggle();
                }}
              >
                <SegmentedControlItem value="light" label="Light" />
                <SegmentedControlItem value="dark" label="Dark" />
              </SegmentedControl>
            </HStack>
          </VStack>
        </Card>

        <Card>
          <form onSubmit={submitPassword} noValidate>
            <VStack gap={4}>
              <Heading level={4}>{me.has_password ? "Change password" : "Set a password"}</Heading>
              {me.has_password ? (
                <TextInput label="Current password" type="password" value={current} onChange={setCurrent} isRequired />
              ) : (
                <Text type="supporting" color="secondary">
                  Your account was created by single sign-on and has no password yet. Setting one also lets you
                  sign in with email and password.
                </Text>
              )}
              <TextInput
                label="New password"
                type="password"
                value={next}
                onChange={setNext}
                description="At least 8 characters"
                isRequired
                status={tooShort ? { type: "error", message: "Too short" } : undefined}
              />
              <TextInput
                label="Confirm new password"
                type="password"
                value={confirm}
                onChange={setConfirm}
                isRequired
                status={mismatch ? { type: "error", message: "Passwords do not match" } : undefined}
              />
              <HStack hAlign="end">
                <Button
                  label="Update password"
                  type="submit"
                  variant="primary"
                  isLoading={busy}
                  isDisabled={(me.has_password && !current) || !next || mismatch || tooShort}
                />
              </HStack>
            </VStack>
          </form>
        </Card>

        <Card>
          <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
            <VStack gap={0}>
              <Heading level={4}>Sign out</Heading>
              <Text type="supporting" color="secondary">
                Clears the local token and ends the SSO session.
              </Text>
            </VStack>
            <Button label="Sign out" variant="destructive" onClick={signOut} />
          </HStack>
        </Card>
      </VStack>
    </Page>
  );
}
