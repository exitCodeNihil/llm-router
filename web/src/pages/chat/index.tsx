/**
 * Playground.
 *
 * Full-height, chat-first frame in the style of ChatGPT / LM Studio: the
 * conversation owns the window, and both rails are collapsible so it can go
 * edge to edge. Panel visibility persists across reloads.
 *
 * Responsive contract:
 *   > 1100px  history 260 (toggle) | thread (flex) | settings 340 (toggle)
 *   <= 1100px rails become overlays; only one is open at a time
 *   <= 700px  rails are full-width overlays; the thread keeps the viewport
 *
 * Session state lives in useChatSession; everything below is composition.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Center } from "@astryxdesign/core/Center";
import { Layout, LayoutContent, LayoutHeader, LayoutPanel } from "@astryxdesign/core/Layout";
import { Heading } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { IconButton } from "@astryxdesign/core/IconButton";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Divider } from "@astryxdesign/core/Divider";
import { ChatMessageList } from "@astryxdesign/core/Chat";
import { useMediaQuery } from "@astryxdesign/core/hooks";
import { useToast } from "@astryxdesign/core/Toast";

import { Confirm } from "../../components/dialogs";
import { IconChat, IconPlus, IconSliders } from "../../components/icons";
import { useDeployments } from "../../lib/hooks";
import { useChatSession } from "./useChatSession";
import { pastedTextAttachment, readAttachment, schemaInvalid, type Attachment } from "./wire";
import {
  ChatList,
  Composer,
  ContextMeter,
  CopyAsCurl,
  ModelSwitcher,
  SendAsKey,
  MessageView,
  SettingsPanel,
  StreamingMessage,
  ThreadEmptyState,
  ToolResultsPrompt,
  useComposerState,
} from "./parts";

const HISTORY_W = 260;
const SETTINGS_W = 340;
/** Readable measure for the thread, regardless of how wide the window is. */
const THREAD_MAX = 780;

/**
 * Panel visibility, remembered between sessions.
 *
 * `persist` is switched off on narrow viewports so that collapsing a rail to
 * make room for the conversation there does not overwrite the desktop layout
 * the user chose.
 */
function usePanelState(key: string, initial: boolean, persist = true) {
  const [open, setOpen] = useState(() => {
    const saved = localStorage.getItem(key);
    return saved === null ? initial : saved === "1";
  });
  useEffect(() => {
    if (persist) localStorage.setItem(key, open ? "1" : "0");
  }, [key, open, persist]);
  return [open, setOpen] as const;
}

export default function Chat() {
  const s = useChatSession();
  const showToast = useToast();
  const { draft, setDraft, attachments, setAttachments } = useComposerState();
  const [deleting, setDeleting] = useState("");

  const wide = useMediaQuery("(min-width: 1100px)");
  const narrow = useMediaQuery("(max-width: 700px)");

  const [historyOpen, setHistoryOpen] = usePanelState("llmr_chat_history", true, !narrow);
  const [settingsOpen, setSettingsOpen] = usePanelState("llmr_chat_settings", false, !narrow);

  // Entering a narrow viewport: collapse both rails so the conversation is what
  // you land on. Runs only on the transition, so a rail opened afterwards stays
  // open. Not persisted (see usePanelState).
  useEffect(() => {
    if (narrow) {
      setHistoryOpen(false);
      setSettingsOpen(false);
    }
  }, [narrow, setHistoryOpen, setSettingsOpen]);

  // Keep the newest content in view while a reply streams in.
  const threadRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = threadRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [s.messages, s.streamText, s.streamReason]);

  // Below the wide breakpoint the rails overlay the thread, so only one may be
  // open at a time — otherwise they stack and bury the conversation.
  const openHistory = (next: boolean) => {
    setHistoryOpen(next);
    if (next && !wide) setSettingsOpen(false);
  };
  const openSettings = (next: boolean) => {
    setSettingsOpen(next);
    if (next && !wide) setHistoryOpen(false);
  };

  const addFileList = useCallback(
    async (files: File[]) => {
      for (const file of files) {
        const result = await readAttachment(file);
        if ("error" in result) showToast({ body: result.error, type: "error" });
        else setAttachments((prev) => [...prev, result.attachment]);
      }
    },
    [setAttachments, showToast],
  );

  const addFiles = useCallback(
    (list: FileList | null) => addFileList(Array.from(list ?? [])),
    [addFileList],
  );

  // A large paste becomes an attachment instead of flooding the composer.
  const addPastedText = useCallback(
    (text: string) => {
      setAttachments((prev) => [
        ...prev,
        pastedTextAttachment(text, prev.filter((a) => a.name.startsWith("pasted-")).length + 1),
      ]);
      showToast({ body: "Long paste attached as a text file", uniqueID: "paste-attached" });
    },
    [setAttachments, showToast],
  );

  const submit = useCallback(
    async (value: string) => {
      const pending: Attachment[] = attachments;
      setDraft("");
      setAttachments([]);
      await s.send(value, pending);
    },
    [attachments, s, setAttachments, setDraft],
  );

  const models = s.models.data ?? [];

  // Which provider(s) back each alias — shown in the model switcher so it is
  // obvious where a request will actually land, and whether failover exists.
  const deployments = useDeployments();
  const routes = useMemo(() => {
    const m = new Map<string, string[]>();
    for (const d of deployments.data ?? []) {
      if (!d.enabled) continue;
      m.set(d.model_name, [...(m.get(d.model_name) ?? []), d.provider_name]);
    }
    return m;
  }, [deployments.data]);
  const blockedBySchema = schemaInvalid(s.settings);
  // Must be a real boolean: ChatLayout only treats null/false as "empty", so a
  // falsy chain ending in s.error ("") would suppress the empty state.
  const hasThreadContent = s.messages.length > 0 || s.busy || Boolean(s.error);

  // ── Rails ─────────────────────────────────────────────────────
  const historyPanel = (
    <LayoutPanel
      width={narrow ? "100%" : HISTORY_W}
      hasDivider
      isScrollable
      padding={3}
      label="Saved conversations"
    >
      <VStack gap={3}>
        <HStack hAlign="between" vAlign="center">
          <span className="eyebrow">History</span>
          <Button
            label="New"
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconPlus} size="sm" />}
            onClick={() => {
              s.reset();
              setDraft("");
              setAttachments([]);
              if (!wide) openHistory(false);
            }}
          />
        </HStack>
        <ChatList
          chats={s.chats.data ?? []}
          activeId={s.activeId}
          saveBlocked={!s.canPersist}
          saveFailed={s.saveFailed}
          onOpen={(id) => {
            s.open(id);
            if (!wide) openHistory(false);
          }}
          onDelete={setDeleting}
        />
      </VStack>
    </LayoutPanel>
  );

  const settingsPanel = (
    <LayoutPanel
      width={narrow ? "100%" : SETTINGS_W}
      hasDivider
      isScrollable
      padding={4}
      label="Request settings"
    >
      <VStack gap={4}>
        <HStack hAlign="between" vAlign="center">
          <span className="eyebrow">Request</span>
          <IconButton
            label="Hide request settings"
            variant="ghost"
            size="sm"
            icon={<Icon icon="close" size="sm" />}
            onClick={() => openSettings(false)}
          />
        </HStack>
        <ContextMeter used={s.usedTokens} limit={s.contextTokens} />

        <SettingsPanel
          models={models}
          systemPrompt={s.systemPrompt}
          onSystemPromptChange={s.setSystemPrompt}
          settings={s.settings}
          onSettingsChange={s.setSettings}
          isModelsLoading={s.models.isLoading}
        />

        <Divider />
        <SendAsKey sendAsKey={s.sendAsKey} onChange={s.setSendAsKey} />

        <Divider />
        <CopyAsCurl
          model={s.model}
          systemPrompt={s.systemPrompt}
          messages={s.messages}
          settings={s.settings}
        />
      </VStack>
    </LayoutPanel>
  );

  // ── Header ────────────────────────────────────────────────────
  const header = (
    <LayoutHeader hasDivider>
      <HStack gap={2} vAlign="center" hAlign="between" paddingInline={3} paddingBlock={2}>
        <HStack gap={2} vAlign="center">
          <IconButton
            label={historyOpen ? "Hide history" : "Show history"}
            variant="ghost"
            size="sm"
            icon={<Icon icon={IconChat} size="sm" />}
            onClick={() => openHistory(!historyOpen)}
          />
          <Heading level={5}>Playground</Heading>
          {s.model && <Badge variant="neutral" label={s.model} />}
          {s.busy && <Badge variant="blue" label="streaming" />}
          {s.sendAsKey && <Badge variant="purple" label="as key" />}
          {blockedBySchema && <Badge variant="warning" label="schema invalid" />}
        </HStack>

        <HStack gap={2} vAlign="center">
          {!narrow && s.messages.length > 0 && (
            <Button
              label="New chat"
              variant="secondary"
              size="sm"
              icon={<Icon icon={IconPlus} size="sm" />}
              onClick={() => {
                s.reset();
                setDraft("");
                setAttachments([]);
              }}
            />
          )}
          <IconButton
            label={settingsOpen ? "Hide request settings" : "Show request settings"}
            variant={settingsOpen ? "secondary" : "ghost"}
            size="sm"
            icon={<Icon icon={IconSliders} size="sm" />}
            onClick={() => openSettings(!settingsOpen)}
          />
        </HStack>
      </HStack>
    </LayoutHeader>
  );

  // ── Thread ────────────────────────────────────────────────────
  //
  // Composed directly rather than with ChatLayout: that component keeps its
  // composer dock inside its own scroll container and gives the message area
  // min-height:100%, so the content always overflows by the dock's height and
  // its scroll-to-bottom then hides short threads. A scrolling thread plus a
  // fixed composer below it is what a full-height frame actually needs.
  const measure = { width: "100%", maxWidth: THREAD_MAX, margin: "0 auto" } as const;

  const thread = (
    <LayoutContent padding={0} label="Conversation" isScrollable={false}>
      <div style={{ display: "flex", flexDirection: "column", height: "100%", minHeight: 0 }}>
        <div ref={threadRef} style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
          {hasThreadContent ? (
            <div style={{ ...measure, paddingBlock: "var(--spacing-4)" }}>
              <ChatMessageList isStreaming={s.busy} density="balanced">
                {s.messages.map((m, i) => (
                  <MessageView
                    key={i}
                    message={m}
                    model={s.model}
                    onRegenerate={s.regenerate}
                    isLast={i === s.messages.length - 1 && !s.busy}
                  />
                ))}
                {s.busy && (
                  <StreamingMessage text={s.streamText} reasoning={s.streamReason} model={s.model} />
                )}
                {s.error && <Banner status="error" title="Request failed" description={s.error} />}
                {s.pendingCalls && (
                  <ToolResultsPrompt
                    calls={s.pendingCalls}
                    values={s.toolResults}
                    onChange={s.setToolResults}
                    onSubmit={() => s.pendingCalls && void s.sendToolResults(s.pendingCalls)}
                  />
                )}
              </ChatMessageList>
            </div>
          ) : (
            <Center minHeight="100%">
              <ThreadEmptyState noModels={!s.models.isLoading && models.length === 0} />
            </Center>
          )}
        </div>

        {/* Composer: outside the scroller, so it never scrolls away. */}
        <div style={{ paddingInline: "var(--spacing-3)", paddingBottom: "var(--spacing-3)" }}>
          <div style={measure}>
            <Composer
              draft={draft}
              onDraftChange={setDraft}
              attachments={attachments}
              onAttach={addFiles}
              onAttachFiles={addFileList}
              onPasteText={addPastedText}
              onRemoveAttachment={(i) => setAttachments((a) => a.filter((_, j) => j !== i))}
              onSubmit={submit}
              onStop={s.stop}
              isStreaming={s.busy}
              isDisabled={models.length === 0 || blockedBySchema}
              model={s.model}
              modelSwitcher={
                <ModelSwitcher
                  models={models}
                  model={s.model}
                  onChange={s.setModel}
                  routes={routes}
                  isDisabled={s.busy}
                />
              }
            />
          </div>
        </div>
      </div>
    </LayoutContent>
  );

  // Narrow screens: a rail replaces the thread rather than squeezing it.
  const railInsteadOfThread = narrow && (historyOpen || settingsOpen);

  return (
    <>
      <Layout
        height="fill"
        header={header}
        start={!railInsteadOfThread && historyOpen ? historyPanel : undefined}
        content={
          railInsteadOfThread ? (
            historyOpen ? (
              historyPanel
            ) : (
              settingsPanel
            )
          ) : (
            thread
          )
        }
        end={!railInsteadOfThread && settingsOpen ? settingsPanel : undefined}
      />

      <Confirm
        target={deleting}
        title="Delete chat"
        description={`Delete "${s.chats.data?.find((c) => c.id === deleting)?.title ?? "this conversation"}"? This cannot be undone.`}
        isLoading={s.isDeleting}
        onCancel={() => setDeleting("")}
        onConfirm={async () => {
          await s.remove(deleting);
          setDeleting("");
        }}
      />
    </>
  );
}
