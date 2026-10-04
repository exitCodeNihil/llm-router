import { useEffect, useState } from "react";
import { Center } from "@astryxdesign/core/Center";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { Divider } from "@astryxdesign/core/Divider";
import { Spinner } from "@astryxdesign/core/Spinner";

import { api, ApiError, token } from "../lib/api";
import type { Me } from "../lib/types";
import { IconSignal } from "../components/icons";

type Mode = "loading" | "setup" | "signin";

export default function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [mode, setMode] = useState<Mode>("loading");
  const [sso, setSso] = useState(false);
  const [showToken, setShowToken] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [tokenValue, setTokenValue] = useState("");

  // First run? The setup screen replaces the login form until an admin exists.
  useEffect(() => {
    api
      .get<{ needed: boolean; sso?: boolean }>("/auth/setup")
      .then((r) => {
        setSso(!!r.sso);
        setMode(r.needed ? "setup" : "signin");
      })
      .catch(() => setMode("signin"));
  }, []);

  const fail = (err: unknown, unauthorized: string) =>
    setError(
      err instanceof ApiError && err.status === 401
        ? unauthorized
        : err instanceof Error
          ? err.message
          : "Sign-in failed.",
    );

  async function submitSetup(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.post("/auth/setup", { name: name.trim(), email: email.trim(), password });
      onSignedIn(); // session cookie is now set
    } catch (err) {
      fail(err, "Setup failed.");
      setBusy(false);
    }
  }

  async function submitPassword(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.post("/auth/password", { email: email.trim(), password });
      onSignedIn();
    } catch (err) {
      fail(err, "Incorrect email or password.");
      setBusy(false);
    }
  }

  async function submitToken(e: React.FormEvent) {
    e.preventDefault();
    if (!tokenValue.trim()) return;
    setBusy(true);
    setError(null);
    token.set(tokenValue.trim());
    try {
      await api.get<Me>("/api/me"); // validate before committing
      onSignedIn();
    } catch (err) {
      token.clear();
      fail(err, "That token was not accepted.");
      setBusy(false);
    }
  }

  return (
    <Center minHeight="100dvh">
      <VStack gap={6} width="100%" maxWidth={400} padding={4}>
        <VStack gap={2} hAlign="center">
          <IconSignal width={34} height={34} />
          <Heading level={3}>llm-router</Heading>
          <Text type="body" color="secondary">
            {mode === "setup" ? "Create the first administrator" : "Sign in to the gateway console"}
          </Text>
        </VStack>

        <Card padding={5}>
          {mode === "loading" ? (
            <Center minHeight={180}>
              <Spinner size="md" label="Connecting" />
            </Center>
          ) : mode === "setup" ? (
            <form onSubmit={submitSetup} noValidate>
              <VStack gap={4}>
                <TextInput label="Name" value={name} onChange={setName} placeholder="Ada Lovelace" isRequired hasAutoFocus />
                <TextInput
                  label="Email"
                  type="email"
                  value={email}
                  onChange={setEmail}
                  placeholder="admin@company.com"
                  isRequired
                />
                <TextInput
                  label="Password"
                  type="password"
                  value={password}
                  onChange={setPassword}
                  description="At least 8 characters"
                  isRequired
                  status={
                    password && password.length < 8
                      ? { type: "error", message: "Too short" }
                      : undefined
                  }
                />
                {error && <Banner status="error" title="Setup failed" description={error} />}
                <Button
                  label="Set up llm-router"
                  type="submit"
                  variant="primary"
                  isLoading={busy}
                  isDisabled={!name.trim() || !email.trim() || password.length < 8}
                  width="100%"
                />
              </VStack>
            </form>
          ) : (
            <VStack gap={4}>
              <form onSubmit={submitPassword} noValidate>
                <VStack gap={4}>
                  <TextInput
                    label="Email"
                    type="email"
                    value={email}
                    onChange={setEmail}
                    placeholder="you@company.com"
                    isRequired
                    hasAutoFocus
                  />
                  <TextInput label="Password" type="password" value={password} onChange={setPassword} isRequired />
                  {error && <Banner status="error" title="Could not sign in" description={error} />}
                  <Button
                    label="Sign in"
                    type="submit"
                    variant="primary"
                    isLoading={busy}
                    isDisabled={!email.trim() || !password}
                    width="100%"
                  />
                </VStack>
              </form>

              <HStack gap={3} vAlign="center">
                <div style={{ flex: 1 }}>
                  <Divider />
                </div>
                <span className="eyebrow">or</span>
                <div style={{ flex: 1 }}>
                  <Divider />
                </div>
              </HStack>

              {sso && (
                <Button
                  label="Sign in with SSO"
                  variant="secondary"
                  width="100%"
                  onClick={() => {
                    window.location.href = "/auth/login";
                  }}
                />
              )}

              <Center>
                <Button
                  label={showToken ? "Hide admin token" : "Use admin token"}
                  variant="ghost"
                  size="sm"
                  onClick={() => setShowToken((v) => !v)}
                />
              </Center>

              {showToken && (
                <form onSubmit={submitToken}>
                  <VStack gap={3}>
                    <Divider />
                    <TextInput
                      label="Bootstrap admin token"
                      type="password"
                      value={tokenValue}
                      onChange={setTokenValue}
                      placeholder="LLMR_ADMIN_TOKEN"
                      description="Falls back to the token from the server environment."
                    />
                    <Button
                      label="Continue with token"
                      type="submit"
                      variant="secondary"
                      isLoading={busy}
                      isDisabled={!tokenValue.trim()}
                      width="100%"
                    />
                  </VStack>
                </form>
              )}
            </VStack>
          )}
        </Card>

        <Center>
          <Text type="supporting" color="secondary">
            Self-hosted LLM gateway · one endpoint, every provider
          </Text>
        </Center>
      </VStack>
    </Center>
  );
}
