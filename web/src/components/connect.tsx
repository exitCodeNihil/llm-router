import { useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";

import { useChatModels } from "../lib/hooks";

const KEY_PLACEHOLDER = "llmr_PASTE_YOUR_KEY";

type Harness = "claude" | "opencode" | "pi" | "curl";

const HARNESSES: { value: Harness; label: string }[] = [
  { value: "claude", label: "Claude Code" },
  { value: "opencode", label: "OpenCode" },
  { value: "pi", label: "pi" },
  { value: "curl", label: "cURL" },
];

/**
 * Ready-to-paste client configuration for one key. Every harness gets the
 * key's real model list: Claude Code discovers it live from /v1/models, the
 * others only read a static file, so the file is generated here instead of
 * typed from memory.
 */
function snippets(base: string, key: string, models: string[], subscription: boolean): Record<Harness, string> {
  const first = models[0] ?? "my-model";
  // Claude Code's discovery keeps ids containing "claude" or "anthropic".
  const claude = models.find((m) => /claude|anthropic/i.test(m)) ?? first;
  return {
    claude: (subscription
      ? [
          `# Claude Code keeps using your claude.ai login; the gateway only forwards it.`,
          `# The gateway key rides in X-Llmr-Key — do NOT set ANTHROPIC_AUTH_TOKEN here.`,
          `export ANTHROPIC_BASE_URL=${base}`,
          `export ANTHROPIC_CUSTOM_HEADERS="X-Llmr-Key: ${key}"`,
          `export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`,
          `claude`,
        ]
      : [
          `export ANTHROPIC_BASE_URL=${base}`,
          `export ANTHROPIC_AUTH_TOKEN=${key}`,
          `# /model lists this key's Claude models (ids containing "claude" or "anthropic")`,
          `export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`,
          `export ANTHROPIC_MODEL=${claude}`,
          `claude`,
        ]
    ).join("\n"),
    opencode: JSON.stringify(
      {
        $schema: "https://opencode.ai/config.json",
        provider: {
          "llm-router": {
            npm: "@ai-sdk/openai-compatible",
            name: "llm-router",
            options: { baseURL: `${base}/v1`, apiKey: key },
            models: Object.fromEntries(models.map((m) => [m, { name: m }])),
          },
        },
        model: `llm-router/${first}`,
      },
      null,
      2,
    ),
    pi: JSON.stringify(
      {
        providers: {
          llmr: {
            baseUrl: `${base}/v1`,
            api: "openai-completions",
            apiKey: key,
            models: models.map((id) => ({ id })),
          },
        },
      },
      null,
      2,
    ),
    curl: [
      `curl ${base}/v1/models -H "Authorization: Bearer ${key}"`,
      ``,
      `curl ${base}/v1/chat/completions -H "Authorization: Bearer ${key}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{"model":"${first}","messages":[{"role":"user","content":"hi"}]}'`,
    ].join("\n"),
  };
}

const FILES: Record<Harness, string> = {
  claude: "shell",
  opencode: "opencode.json — in the project or ~/.config/opencode/",
  pi: "~/.pi/agent/models.json",
  curl: "shell",
};

export function ConnectDialog({
  secret,
  allowedModels,
  onClose,
}: {
  /** The full key, known only right after creation. */
  secret?: string;
  /** null means every model the gateway serves. */
  allowedModels: string[] | null;
  onClose: () => void;
}) {
  const all = useChatModels();
  const [harness, setHarness] = useState<Harness>("claude");
  const [subscription, setSubscription] = useState(false);
  const models = [...(allowedModels ?? all.data ?? [])].sort();
  const base = window.location.origin;
  const code = snippets(base, secret ?? KEY_PLACEHOLDER, models, subscription);

  return (
    <Dialog
      isOpen
      onOpenChange={(open) => !open && onClose()}
      purpose={secret ? "required" : "info"}
      width={680}
      maxHeight="calc(100vh - 48px)"
    >
      <DialogHeader
        title={secret ? "API key created" : "Connect a client"}
        subtitle={
          secret
            ? "This is the only time the full key is shown — it is already filled into the snippets below."
            : "Configuration for this key, with every model it may call."
        }
      />
      <VStack gap={4} padding={4} isScrollable style={{ minHeight: 0 }}>
        {secret && (
          <>
            <Banner
              status="warning"
              title="Copy it now"
              description="The gateway stores only a hash. If you lose this value you must issue a new key."
            />
            <CodeBlock code={secret} language="plaintext" width="100%" isWrapped />
          </>
        )}
        <SegmentedControl label="Client" size="sm" value={harness} onChange={(v) => setHarness(v as Harness)}>
          {HARNESSES.map((h) => (
            <SegmentedControlItem key={h.value} value={h.value} label={h.label} />
          ))}
        </SegmentedControl>
        {harness === "claude" && (
          <SegmentedControl
            label="Credential"
            size="sm"
            value={subscription ? "subscription" : "key"}
            onChange={(v) => setSubscription(v === "subscription")}
          >
            <SegmentedControlItem value="key" label="Gateway key pays (API providers)" />
            <SegmentedControlItem value="subscription" label="My claude.ai subscription (pass-through)" />
          </SegmentedControl>
        )}
        <CodeBlock
          code={code[harness]}
          language={harness === "opencode" || harness === "pi" ? "json" : "bash"}
          title={FILES[harness]}
          width="100%"
          maxHeight={360}
        />
        <Text type="supporting" color="secondary">
          {models.length === 0
            ? "No models are exposed yet — add one on the Models page and reopen this."
            : `${models.length} model${models.length === 1 ? "" : "s"}: ${models.join(", ")}`}
        </Text>
        <HStack gap={2} hAlign="end">
          <Button label="Done" variant="primary" onClick={onClose} />
        </HStack>
      </VStack>
    </Dialog>
  );
}
