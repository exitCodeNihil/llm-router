/**
 * Presentation pieces for the playground. Every component here is driven
 * purely by props so they can be rearranged or reused without touching the
 * session logic in useChatSession.
 */
import { useRef, useState, type ReactNode } from "react";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { List, ListItem } from "@astryxdesign/core/List";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Token } from "@astryxdesign/core/Token";
import { Avatar } from "@astryxdesign/core/Avatar";
import { Banner } from "@astryxdesign/core/Banner";
import { Card } from "@astryxdesign/core/Card";
import { Divider } from "@astryxdesign/core/Divider";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { Code } from "@astryxdesign/core/Code";
import { useToast } from "@astryxdesign/core/Toast";
import { Markdown } from "@astryxdesign/core/Markdown";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import {
  DropdownMenu,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
} from "@astryxdesign/core/DropdownMenu";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import { NumberInput } from "@astryxdesign/core/NumberInput";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@astryxdesign/core/SegmentedControl";
import {
  ChatComposer,
  ChatComposerInput,
  ChatMessage as ChatMessageRow,
  ChatMessageBubble,
  ChatMessageMetadata,
  ChatSystemMessage,
} from "@astryxdesign/core/Chat";

import type {
  ChatMessage,
  ChatSettings,
  ChatSummary,
  ChatToolCall,
  ChatUITool,
} from "../../lib/types";
import { Link } from "react-router-dom";
import { compact, int, money } from "../../lib/format";
import { IconPlus, IconSignal, IconTrash } from "../../components/icons";
import {
  FILE_ACCEPT,
  PASTE_AS_FILE_CHARS,
  displayTitle,
  isInvalidJson,
  newTool,
  schemaInvalid,
  wireExtras,
  wireMessages,
  type Attachment,
} from "./wire";

// ── Message content ─────────────────────────────────────────────
function MessageContent({ content }: { content: ChatMessage["content"] }) {
  if (content == null) return null;
  if (typeof content === "string") return <Markdown>{content}</Markdown>;
  return (
    <VStack gap={2}>
      {content.map((p, i) =>
        p.type === "text" ? (
          <Markdown key={i}>{p.text}</Markdown>
        ) : (
          <img
            key={i}
            src={p.image_url.url}
            alt="Attached image"
            style={{
              maxHeight: 200,
              maxWidth: "100%",
              objectFit: "contain",
              borderRadius: "var(--radius-element)",
              border: "1px solid var(--color-border)",
            }}
          />
        ),
      )}
    </VStack>
  );
}

function ToolCallView({ call }: { call: ChatToolCall }) {
  return (
    <CodeBlock
      title={call.function.name}
      language="json"
      code={call.function.arguments || "{}"}
      width="100%"
      isWrapped
      size="sm"
    />
  );
}

/**
 * Per-turn readout: what the gateway did. Routing attribution, perceived
 * latency and cost are the reasons an operator uses this playground rather than
 * a consumer chat app, so they sit inline under every answer.
 */
function TurnFooter({ message }: { message: ChatMessage }) {
  const t = message._turn;
  const usage = message._usage;
  if (!t && !usage) return null;

  const bits: string[] = [];
  if (usage)
    bits.push(`${compact(usage.prompt)} in · ${compact(usage.completion)} out`);
  if (t?.costUsd != null && !t.unpriced)
    bits.push(money(t.costUsd, { precise: true }));
  if (t?.ttftMs != null) bits.push(`${int(t.ttftMs)} ms to first token`);
  if (t?.totalMs != null) bits.push(`${int(t.totalMs)} ms total`);

  return (
    <ChatMessageMetadata
      footer={
        <HStack gap={2} vAlign="center" wrap="wrap">
          {bits.length > 0 && (
            <span className="tnum">
              <Text type="supporting" color="secondary">
                {bits.join(" · ")}
              </Text>
            </span>
          )}
          {t?.provider && (
            <Tooltip
              content={`Served by ${t.provider}${t.upstream ? ` → ${t.upstream}` : ""}`}
            >
              <Badge variant="neutral" label={t.provider} />
            </Tooltip>
          )}
          {t?.attempts != null && t.attempts > 1 && (
            <Tooltip
              content={`First ${t.attempts - 1} backend(s) failed; this one served the request`}
            >
              <Badge variant="warning" label={`failover ×${t.attempts - 1}`} />
            </Tooltip>
          )}
          {t?.unpriced && (
            <Tooltip content="No price applies, so the gateway records this request unpriced">
              <Badge variant="warning" label="unpriced" />
            </Tooltip>
          )}
          {t?.sentAsKey && (
            <Tooltip content="Sent as a virtual API key, so that key's limits applied">
              <Badge variant="purple" label={`key ${t.sentAsKey}…`} />
            </Tooltip>
          )}
          {t?.requestId && (
            <Link
              to={`/requests?request_id=${encodeURIComponent(t.requestId)}`}
            >
              <Text type="supporting" color="accent">
                <Code>{t.requestId.slice(0, 8)}</Code>
              </Text>
            </Link>
          )}
        </HStack>
      }
    />
  );
}

/** Copy / regenerate, revealed on the assistant turn. */
function MessageActions({
  text,
  onRegenerate,
  isLast,
}: {
  text: string;
  onRegenerate?: () => void;
  isLast: boolean;
}) {
  const showToast = useToast();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      showToast({ body: "Message copied", uniqueID: "msg-copy" });
    } catch {
      showToast({
        body: "Clipboard unavailable",
        type: "error",
        uniqueID: "msg-copy",
      });
    }
  };
  return (
    <HStack gap={1}>
      <Button
        label="Copy"
        variant="ghost"
        size="sm"
        onClick={copy}
        isDisabled={!text}
      />
      {isLast && onRegenerate && (
        <Button
          label="Regenerate"
          variant="ghost"
          size="sm"
          onClick={onRegenerate}
        />
      )}
    </HStack>
  );
}

export function MessageView({
  message,
  model,
  onRegenerate,
  isLast = false,
}: {
  message: ChatMessage;
  model: string;
  onRegenerate?: () => void;
  isLast?: boolean;
}) {
  if (message.role === "tool") {
    return (
      <ChatSystemMessage>
        <HStack gap={2} vAlign="center">
          <Badge variant="neutral" label="tool result" />
          <Text type="supporting" color="secondary" maxLines={2}>
            {typeof message.content === "string" ? message.content : ""}
          </Text>
        </HStack>
      </ChatSystemMessage>
    );
  }

  if (message.role === "user") {
    return (
      <ChatMessageRow sender="user">
        <ChatMessageBubble>
          <MessageContent content={message.content} />
        </ChatMessageBubble>
      </ChatMessageRow>
    );
  }

  const plain = typeof message.content === "string" ? message.content : "";
  const by = message._model || model;
  return (
    <ChatMessageRow
      sender="assistant"
      avatar={<Avatar name={by || "AI"} size="sm" />}
    >
      <VStack gap={2}>
        {message._reasoning && (
          <Collapsible trigger="Reasoning">
            <Text type="supporting" color="secondary">
              {message._reasoning}
            </Text>
          </Collapsible>
        )}
        <MessageContent content={message.content} />
        {message.tool_calls?.map((tc) => (
          <ToolCallView key={tc.id} call={tc} />
        ))}
        <MessageActions
          text={plain}
          onRegenerate={onRegenerate}
          isLast={isLast}
        />
      </VStack>
      <TurnFooter message={message} />
    </ChatMessageRow>
  );
}

/** The in-flight assistant turn, before it is committed to the transcript. */
export function StreamingMessage({
  text,
  reasoning,
  model,
}: {
  text: string;
  reasoning: string;
  model: string;
}) {
  return (
    <ChatMessageRow
      sender="assistant"
      avatar={<Avatar name={model || "AI"} size="sm" />}
    >
      <VStack gap={2}>
        {reasoning && (
          <Collapsible trigger="Reasoning" defaultIsOpen>
            <Text type="supporting" color="secondary">
              {reasoning}
            </Text>
          </Collapsible>
        )}
        {text ? (
          <Markdown>{text}</Markdown>
        ) : (
          !reasoning && (
            <Text type="supporting" color="secondary">
              Thinking…
            </Text>
          )
        )}
      </VStack>
      <ChatMessageMetadata status="sending" />
    </ChatMessageRow>
  );
}

export function ThreadEmptyState({ noModels }: { noModels?: boolean }) {
  if (noModels) {
    return (
      <EmptyState
        icon={<IconSignal width={28} height={28} />}
        title="No models to chat with yet"
        description="Add a provider, then a model (Providers and Models in the sidebar). The seeded claude-* model is for Claude Code on your claude.ai login, so it is not offered here."
      />
    );
  }
  return (
    <EmptyState
      icon={<IconSignal width={28} height={28} />}
      title="Start a conversation"
      description="Pick a model, set a system prompt if you like, and say something. Traffic from here is metered and billed exactly like production traffic."
    />
  );
}

// ── Pending tool calls ──────────────────────────────────────────
export function ToolResultsPrompt({
  calls,
  values,
  onChange,
  onSubmit,
}: {
  calls: ChatToolCall[];
  values: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
  onSubmit: () => void;
}) {
  return (
    <Card>
      <VStack gap={3}>
        <HStack gap={2} vAlign="center">
          <Badge
            variant="warning"
            label={`${calls.length} tool call${calls.length > 1 ? "s" : ""}`}
          />
          <Text type="supporting" color="secondary">
            Type each result and continue — nothing executes server-side.
          </Text>
        </HStack>
        {calls.map((tc) => (
          <TextArea
            key={tc.id}
            label={tc.function.name}
            rows={2}
            placeholder='e.g. {"ok": true}'
            value={values[tc.id] ?? ""}
            onChange={(v) => onChange({ ...values, [tc.id]: v })}
          />
        ))}
        <HStack hAlign="end">
          <Button
            label="Send tool results"
            variant="primary"
            size="sm"
            onClick={onSubmit}
          />
        </HStack>
      </VStack>
    </Card>
  );
}

// ── Model switcher ──────────────────────────────────────────────
/**
 * Inline model picker, in the composer where the model is actually chosen.
 * Descriptions carry gateway-specific information a consumer chat app has no
 * equivalent for: which provider serves the alias, and whether more than one
 * deployment backs it (i.e. failover is configured).
 */
export function ModelSwitcher({
  models,
  model,
  onChange,
  routes,
  isDisabled,
}: {
  models: string[];
  model: string;
  onChange: (v: string) => void;
  routes: Map<string, string[]>;
  isDisabled?: boolean;
}) {
  const describe = (alias: string) => {
    const providers = routes.get(alias) ?? [];
    if (!providers.length) return "No backend";
    return providers.length > 1
      ? `${providers[0]} + ${providers.length - 1} failover`
      : providers[0];
  };

  return (
    <DropdownMenu
      button={{
        label: model || "Select a model",
        variant: "ghost",
        size: "sm",
        isDisabled: isDisabled || models.length === 0,
      }}
      menuWidth={280}
    >
      <DropdownMenuRadioGroup value={model} onChange={onChange}>
        {models.map((m) => (
          <DropdownMenuRadioItem
            key={m}
            value={m}
            label={m}
            description={describe(m)}
          />
        ))}
      </DropdownMenuRadioGroup>
    </DropdownMenu>
  );
}

// ── Composer ────────────────────────────────────────────────
export function Composer({
  draft,
  onDraftChange,
  attachments,
  onAttach,
  onAttachFiles,
  onPasteText,
  onRemoveAttachment,
  onSubmit,
  onStop,
  isStreaming,
  isDisabled,
  model,
  modelSwitcher,
}: {
  draft: string;
  onDraftChange: (v: string) => void;
  attachments: Attachment[];
  onAttach: (files: FileList | null) => void;
  onAttachFiles: (files: File[]) => void;
  onPasteText: (text: string) => void;
  onRemoveAttachment: (index: number) => void;
  onSubmit: (value: string) => void;
  onStop: () => void;
  isStreaming: boolean;
  isDisabled: boolean;
  model: string;
  modelSwitcher?: ReactNode;
}) {
  const fileRef = useRef<HTMLInputElement>(null);
  const [isDropTarget, setIsDropTarget] = useState(false);

  return (
    // ChatComposerInput implements paste but not drag & drop, so dropping is
    // handled on a wrapper. Files land in the same place either way.
    <div
      onDrop={(e) => {
        setIsDropTarget(false);
        const files = Array.from(e.dataTransfer?.files ?? []);
        if (isDisabled || !files.length) return;
        e.preventDefault();
        onAttachFiles(files);
      }}
      onDragOver={(e) => {
        if (isDisabled || !e.dataTransfer?.types?.includes("Files")) return;
        e.preventDefault();
        setIsDropTarget(true);
      }}
      onDragLeave={() => setIsDropTarget(false)}
      style={{
        borderRadius: "var(--radius-container)",
        outline: isDropTarget ? "2px dashed var(--color-accent)" : undefined,
        outlineOffset: 4,
      }}
    >
      <ChatComposer
        value={draft}
        onChange={onDraftChange}
        onSubmit={onSubmit}
        onStop={onStop}
        isStopShown={isStreaming}
        isDisabled={isDisabled}
        placeholder={
          isDropTarget
            ? "Drop images or text files…"
            : model
              ? `Message ${model}…`
              : isDisabled
                ? "Add a model to start"
                : "Pick a model to start"
        }
        /**
         * Custom input purely to reach ChatComposerInput's onFiles / onPaste.
         * onFiles covers both clipboard files and drag & drop; onPaste lets a
         * very large text paste become an attachment instead of flooding the
         * field. Everything else (value, submit, placeholder) comes from the
         * surrounding ChatComposer via context.
         */
        input={
          <ChatComposerInput
            /**
             * Astryx turns a long paste into an inline "pasted text" chip by
             * default, and that handler runs before any consumer onPaste. Turn
             * it off so a long paste becomes a real attachment instead —
             * consistent with dropping or picking a .txt file.
             */
            pasteAsToken={false}
            onFiles={(files) => {
              if (!isDisabled) onAttachFiles(files);
            }}
            // Returning true tells ChatComposerInput to skip its own text
            // insert; returning nothing lets the paste land in the field.
            onPaste={(_event, text) => {
              if (isDisabled || text.length <= PASTE_AS_FILE_CHARS)
                return false;
              onPasteText(text);
              return true;
            }}
          />
        }
        headerActions={
          <>
            <IconButton
              label="Attach images or text files"
              variant="ghost"
              size="sm"
              icon={<Icon icon={IconPlus} size="sm" />}
              onClick={() => fileRef.current?.click()}
              isDisabled={isDisabled}
            />
            <input
              ref={fileRef}
              type="file"
              multiple
              accept={FILE_ACCEPT}
              style={{ display: "none" }}
              onChange={(e) => {
                onAttach(e.target.files);
                e.target.value = ""; // let the same file be picked twice
              }}
            />
          </>
        }
        headerContext={
          <HStack gap={1.5} vAlign="center">
            {isStreaming && <Badge variant="blue" label="streaming" />}
            {modelSwitcher}
          </HStack>
        }
        drawer={
          attachments.length > 0 ? (
            <HStack gap={1.5} wrap="wrap">
              {attachments.map((a, i) => (
                <Token
                  key={`${a.name}-${i}`}
                  label={a.name}
                  size="sm"
                  icon={
                    <span aria-hidden>{a.kind === "image" ? "🖼" : "📄"}</span>
                  }
                  onRemove={() => onRemoveAttachment(i)}
                />
              ))}
            </HStack>
          ) : undefined
        }
      />
    </div>
  );
}

// ── Saved conversations ─────────────────────────────────────────
export function ChatList({
  chats,
  activeId,
  saveBlocked,
  saveFailed,
  onOpen,
  onDelete,
}: {
  chats: ChatSummary[];
  activeId: string;
  saveBlocked: boolean;
  saveFailed?: boolean;
  onOpen: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  if (saveBlocked) {
    // The bootstrap admin token is not a user, and conversations belong to a
    // user row — so say what to do about it rather than just reporting it.
    return (
      <Banner
        status="info"
        title="Sign in to save chats"
        description="You are using the bootstrap admin token, which has no user account. Sign in with an email and password to keep conversation history."
      />
    );
  }
  const failed = saveFailed ? (
    <Banner
      status="warning"
      title="Changes are not being saved"
      description="Saving this conversation failed. Earlier chats are still here; reload the page to retry."
    />
  ) : null;
  if (!chats.length) {
    return (
      <Text type="supporting" color="secondary">
        Saved chats appear here.
      </Text>
    );
  }
  return (
    <>
      {failed}
      <List density="compact">
        {chats.map((c) => {
          const label = displayTitle(c.title);
          return (
            <ListItem
              key={c.id}
              label={label}
              isSelected={c.id === activeId}
              onClick={() => onOpen(c.id)}
              endContent={
                <IconButton
                  label={`Delete ${label}`}
                  variant="ghost"
                  size="sm"
                  icon={<Icon icon={IconTrash} size="sm" />}
                  onClick={(e) => {
                    e.stopPropagation();
                    onDelete(c.id);
                  }}
                />
              }
            />
          );
        })}
      </List>
    </>
  );
}

// ── Tool editor ─────────────────────────────────────────────────
function ToolEditor({
  tool,
  onChange,
  onRemove,
}: {
  tool: ChatUITool;
  onChange: (t: ChatUITool) => void;
  onRemove: () => void;
}) {
  const paramsInvalid = isInvalidJson(tool.parameters);
  return (
    <Card variant="muted" padding={3}>
      <VStack gap={2}>
        <HStack gap={2} vAlign="end">
          <TextInput
            label="Name"
            size="sm"
            value={tool.name}
            onChange={(v) => onChange({ ...tool, name: v })}
            placeholder="tool_name"
          />
          <IconButton
            label={`Remove tool ${tool.name || ""}`}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconTrash} size="sm" />}
            onClick={onRemove}
          />
        </HStack>
        <TextInput
          label="Description"
          size="sm"
          value={tool.description}
          onChange={(v) => onChange({ ...tool, description: v })}
          placeholder="What this tool does"
        />
        <TextArea
          label="Parameters (JSON Schema)"
          rows={3}
          value={tool.parameters}
          onChange={(v) => onChange({ ...tool, parameters: v })}
          placeholder='{"type":"object","properties":{}}'
          status={
            paramsInvalid
              ? { type: "error", message: "Not valid JSON" }
              : undefined
          }
        />
      </VStack>
    </Card>
  );
}

// ── Settings panel ──────────────────────────────────────────────
export function SettingsPanel({
  models,
  systemPrompt,
  onSystemPromptChange,
  settings,
  onSettingsChange,
  isModelsLoading,
}: {
  models: string[];
  systemPrompt: string;
  onSystemPromptChange: (v: string) => void;
  settings: ChatSettings;
  onSettingsChange: (s: ChatSettings) => void;
  isModelsLoading: boolean;
}) {
  const patch = (p: Partial<ChatSettings>) =>
    onSettingsChange({ ...settings, ...p });
  const noModels = !isModelsLoading && models.length === 0;

  return (
    // The model lives in the composer (see ModelSwitcher) — one place to change
    // it, next to where you send the message.
    <VStack gap={4}>
      <TextArea
        label="System prompt"
        rows={4}
        value={systemPrompt}
        onChange={onSystemPromptChange}
        placeholder="You are a helpful assistant…"
        isOptional
      />

      <Grid columns={{ minWidth: 130, repeat: "fit" }} gap={3}>
        <NumberInput
          label="Temperature"
          value={settings.temperature}
          onChange={(v) => patch({ temperature: v })}
          min={0}
          max={2}
          step={0.1}
          placeholder="default"
          description="Blank = provider default"
        />
        <NumberInput
          label="Max tokens"
          value={settings.max_tokens}
          onChange={(v) => patch({ max_tokens: v })}
          min={1}
          placeholder="default"
          description="Blank = provider default"
        />
      </Grid>

      <VStack gap={2}>
        <span className="eyebrow">Structured output</span>
        <SegmentedControl
          label="Response format"
          size="sm"
          layout="fill"
          value={settings.response_format}
          onChange={(v) =>
            patch({ response_format: v as ChatSettings["response_format"] })
          }
        >
          <SegmentedControlItem value="none" label="None" />
          <SegmentedControlItem value="json_object" label="JSON" />
          <SegmentedControlItem value="json_schema" label="Schema" />
        </SegmentedControl>
      </VStack>

      {settings.response_format === "json_schema" && (
        <TextArea
          label="JSON schema"
          rows={6}
          value={settings.json_schema}
          onChange={(v) => patch({ json_schema: v })}
          placeholder='{"type":"object","properties":{},"required":[]}'
          status={
            schemaInvalid(settings)
              ? { type: "error", message: "Not valid JSON" }
              : undefined
          }
        />
      )}

      <Divider />

      <HStack hAlign="between" vAlign="center">
        <span className="eyebrow">Tools</span>
        <Button
          label="Add tool"
          variant="ghost"
          size="sm"
          icon={<Icon icon={IconPlus} size="sm" />}
          onClick={() => patch({ tools: [...settings.tools, newTool()] })}
        />
      </HStack>

      {settings.tools.length > 0 && (
        <VStack gap={2}>
          <span className="eyebrow">Tool choice</span>
          <SegmentedControl
            label="Tool choice"
            size="sm"
            layout="fill"
            value={settings.tool_choice}
            onChange={(v) =>
              patch({ tool_choice: v as ChatSettings["tool_choice"] })
            }
          >
            <SegmentedControlItem value="auto" label="Auto" />
            <SegmentedControlItem value="required" label="Required" />
            <SegmentedControlItem value="none" label="None" />
          </SegmentedControl>
        </VStack>
      )}

      {settings.tools.map((t, i) => (
        <ToolEditor
          key={i}
          tool={t}
          onChange={(nt) =>
            patch({ tools: settings.tools.map((x, j) => (j === i ? nt : x)) })
          }
          onRemove={() =>
            patch({ tools: settings.tools.filter((_, j) => j !== i) })
          }
        />
      ))}

      {noModels && (
        <Banner
          status="warning"
          title="No models available"
          description="Add a provider and a model before using the playground."
        />
      )}
    </VStack>
  );
}

// ── Send-as-key ─────────────────────────────────────────────────
/**
 * Which credential the turn is sent with.
 *
 * The console session bypasses key policy entirely, so it cannot answer "does
 * this key's allowed-models list / budget / rate limit actually work?".
 * Pasting a virtual key routes the turn through /v1/chat/completions instead,
 * which is the same surface a real client hits.
 */
export function SendAsKey({
  sendAsKey,
  onChange,
}: {
  sendAsKey: string;
  onChange: (key: string) => void;
}) {
  const [draft, setDraft] = useState(sendAsKey);
  const active = Boolean(sendAsKey);

  return (
    <VStack gap={2}>
      <HStack hAlign="between" vAlign="center">
        <span className="eyebrow">Send as</span>
        {active ? (
          <Badge variant="purple" label="virtual key" />
        ) : (
          <Badge variant="neutral" label="console session" />
        )}
      </HStack>

      <Text type="supporting" color="secondary">
        {active
          ? "Requests go through /v1/chat/completions, so this key's allowed models, budget and rate limits apply."
          : "The console session bypasses key policy. Paste a virtual key to test a client's real restrictions."}
      </Text>

      <TextInput
        label="API key"
        isLabelHidden
        type="password"
        size="sm"
        value={draft}
        onChange={setDraft}
        placeholder="llmr_…"
        hasClear
        description="Held for this browser session only"
      />
      <HStack gap={2}>
        <Button
          label={active ? "Update key" : "Use this key"}
          variant="secondary"
          size="sm"
          isDisabled={!draft.trim() || draft === sendAsKey}
          onClick={() => onChange(draft.trim())}
        />
        {active && (
          <Button
            label="Use session"
            variant="ghost"
            size="sm"
            onClick={() => {
              setDraft("");
              onChange("");
            }}
          />
        )}
      </HStack>
    </VStack>
  );
}

// ── Context meter ───────────────────────────────────────────────
/**
 * How full the context window is. Only shown when an operator has set
 * context_tokens on the deployment, since no provider reports it reliably.
 */
export function ContextMeter({
  used,
  limit,
}: {
  used: number;
  limit: number | null;
}) {
  if (!limit || limit <= 0) return null;
  const pct = Math.min(100, (used / limit) * 100);
  const tone = pct >= 90 ? "error" : pct >= 70 ? "warning" : undefined;

  return (
    <VStack gap={1}>
      <HStack hAlign="between" vAlign="center">
        <span className="eyebrow">Context</span>
        <span className="tnum">
          <Text
            type="supporting"
            color={tone === "error" ? "accent" : "secondary"}
          >
            {compact(used)} / {compact(limit)} ({pct.toFixed(0)}%)
          </Text>
        </span>
      </HStack>
      <ProgressBar
        value={used}
        max={limit}
        label={`Context window ${pct.toFixed(0)} percent used`}
        isLabelHidden
        variant={tone}
      />
      {pct >= 90 && (
        <Text type="supporting" color="secondary">
          Close to the limit — start a new chat to avoid truncation.
        </Text>
      )}
    </VStack>
  );
}

// ── Copy as curl ────────────────────────────────────────────────
/**
 * Reproduce the exact call outside the console. The key is left as a shell
 * variable so the snippet can be shared without leaking a credential.
 */
export function CopyAsCurl({
  model,
  systemPrompt,
  messages,
  settings,
}: {
  model: string;
  systemPrompt: string;
  messages: ChatMessage[];
  settings: ChatSettings;
}) {
  const showToast = useToast();

  const build = () => {
    const extras = wireExtras(settings);
    const body = {
      model,
      messages: wireMessages(systemPrompt, messages),
      stream: true,
      ...("extras" in extras ? extras.extras : {}),
    };
    const json = JSON.stringify(body, null, 2).replace(/'/g, `'\\''`);
    return [
      `curl ${window.location.origin}/v1/chat/completions \\`,
      `  -H "Authorization: Bearer $LLMR_API_KEY" \\`,
      `  -H 'Content-Type: application/json' \\`,
      `  -d '${json}'`,
    ].join("\n");
  };

  return (
    <Button
      label="Copy as curl"
      variant="secondary"
      size="sm"
      isDisabled={!model || messages.length === 0}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(build());
          showToast({
            body: "curl copied — set $LLMR_API_KEY to run it",
            uniqueID: "curl",
          });
        } catch {
          showToast({
            body: "Clipboard unavailable",
            type: "error",
            uniqueID: "curl",
          });
        }
      }}
    />
  );
}

/** Local draft + attachment state, kept out of the session hook. */
export function useComposerState() {
  const [draft, setDraft] = useState("");
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  return { draft, setDraft, attachments, setAttachments };
}
