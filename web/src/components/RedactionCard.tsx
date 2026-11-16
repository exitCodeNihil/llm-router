import { useEffect, useRef, useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Code } from "@astryxdesign/core/Code";
import { Switch } from "@astryxdesign/core/Switch";
import { Divider } from "@astryxdesign/core/Divider";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import { useToast } from "@astryxdesign/core/Toast";

import type { Redaction, RedactionKind } from "../lib/types";
import { useSetTelemetry, useTelemetry, useTestRedaction } from "../lib/hooks";
import { IconPlus, IconTrash } from "./icons";

const KINDS: { value: RedactionKind; label: string; description: string }[] = [
  { value: "email", label: "Email addresses", description: "jane@example.com" },
  { value: "card", label: "Card numbers", description: "13 to 19 digits that pass the card check" },
  { value: "secret", label: "API keys and tokens", description: "Bearer tokens, sk-… keys, cloud and Git tokens, JWTs" },
  { value: "private_key", label: "Private keys", description: "PEM blocks" },
];
const ALL = KINDS.map((k) => k.value);

const same = (a: Redaction, b: Redaction) => JSON.stringify(a) === JSON.stringify(b);

// Redaction is part of the capture policy but saved on its own: it changes
// what leaves, not which requests do.
export function RedactionCard() {
  const tel = useTelemetry();
  const save = useSetTelemetry();
  const test = useTestRedaction();
  const showToast = useToast();

  const [draft, setDraft] = useState<Redaction>({ enabled: false, builtin: null, custom: [] });
  const [sample, setSample] = useState("");
  const [result, setResult] = useState<string | null>(null);

  const hydrated = useRef(false);
  useEffect(() => {
    if (!tel.data || hydrated.current) return;
    hydrated.current = true;
    setDraft(tel.data.redaction);
  }, [tel.data]);

  const saved = tel.data?.redaction;
  if (!tel.data || !saved) return null;

  const kinds = draft.builtin ?? ALL;
  const setKind = (k: RedactionKind, on: boolean) =>
    setDraft({ ...draft, builtin: ALL.filter((v) => (v === k ? on : kinds.includes(v))) });
  const setCustom = (i: number, patch: Partial<Redaction["custom"][number]>) =>
    setDraft({ ...draft, custom: draft.custom.map((c, j) => (j === i ? { ...c, ...patch } : c)) });

  async function apply() {
    try {
      // The policy as stored, with only the redaction replaced.
      await save.mutateAsync({ ...tel.data!, redaction: draft });
      showToast({ body: "Redaction applied" });
    } catch (err) {
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  async function run() {
    try {
      setResult((await test.mutateAsync({ redaction: draft, text: sample })).text);
    } catch (err) {
      setResult(null);
      showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
    }
  }

  return (
    <Card>
      <VStack gap={4}>
        <VStack gap={0}>
          <Heading level={4}>Redaction</Heading>
          <Text type="supporting" color="secondary">
            Replaces personal data and secrets in captured prompts and responses before any destination receives them.
            Images and documents are never exported.
          </Text>
        </VStack>

        <Switch
          label="Redact captured content"
          description="Matches become [REDACTED:kind]. Applies to every destination that takes content."
          value={draft.enabled}
          onChange={(enabled) => setDraft({ ...draft, enabled })}
        />

        {draft.enabled && (
          <VStack gap={3}>
            <VStack gap={2}>
              <span className="eyebrow">Built-in</span>
              {KINDS.map((k) => (
                <Switch
                  key={k.value}
                  label={k.label}
                  description={k.description}
                  value={kinds.includes(k.value)}
                  onChange={(on) => setKind(k.value, on)}
                />
              ))}
            </VStack>

            <VStack gap={2}>
              <span className="eyebrow">Your patterns</span>
              <Text type="supporting" color="secondary">
                Regular expressions in RE2 syntax, such as <Code>EMP-\d{"{6}"}</Code>. A pattern that does not compile
                is refused.
              </Text>
              {draft.custom.map((c, i) => (
                <HStack key={i} gap={2} vAlign="end" wrap="wrap">
                  <TextInput label="Name" value={c.name} onChange={(name) => setCustom(i, { name })} placeholder="employee_id" />
                  <div style={{ flex: 1, minWidth: 240 }}>
                    <TextInput
                      label="Pattern"
                      value={c.pattern}
                      onChange={(pattern) => setCustom(i, { pattern })}
                      placeholder="EMP-\d{6}"
                    />
                  </div>
                  <IconButton
                    label="Remove pattern"
                    icon={<Icon icon={IconTrash} size="sm" />}
                    variant="ghost"
                    onClick={() => setDraft({ ...draft, custom: draft.custom.filter((_, j) => j !== i) })}
                  />
                </HStack>
              ))}
              <HStack>
                <Button
                  label="Add pattern"
                  variant="secondary"
                  icon={<Icon icon={IconPlus} size="sm" />}
                  onClick={() => setDraft({ ...draft, custom: [...draft.custom, { name: "", pattern: "" }] })}
                />
              </HStack>
            </VStack>

            <Divider />

            <VStack gap={2}>
              <span className="eyebrow">Try it</span>
              <TextArea
                label="Sample text"
                value={sample}
                onChange={setSample}
                rows={3}
                placeholder="Contact jane@example.com with key sk-…"
                description="Checked against the settings above, saved or not. The sample is not stored."
              />
              <HStack>
                <Button label="Test" variant="secondary" onClick={run} isDisabled={!sample.trim() || test.isPending} />
              </HStack>
              {result !== null && <Code>{result}</Code>}
            </VStack>
          </VStack>
        )}

        {!same(draft, saved) && (
          <HStack gap={2} hAlign="end">
            <Button label="Discard" variant="ghost" onClick={() => setDraft(saved)} />
            <Button label="Apply" onClick={apply} isDisabled={save.isPending} />
          </HStack>
        )}
      </VStack>
    </Card>
  );
}
