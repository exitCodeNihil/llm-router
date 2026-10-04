/**
 * Playground session state: transcript, streaming turn, tool loop, and
 * server-side persistence.
 *
 * All the moving parts live here so the page component stays declarative.
 * The hook owns exactly one in-flight request at a time via `abortRef`.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useToast } from "@astryxdesign/core/Toast";

import {
  useChat,
  useChatModels,
  useChats,
  useCreateChat,
  useDeleteChat,
  useDeployments,
  useMe,
  usePrices,
  useUpdateChat,
} from "../../lib/hooks";
import type { ChatMessage, ChatSettings, ChatToolCall, TurnMetrics } from "../../lib/types";
import { postStream } from "../../lib/stream";
import { estimateCost } from "../../lib/cost";
import {
  DEFAULT_SETTINGS,
  buildUserContent,
  deriveTitle,
  wireExtras,
  wireMessages,
  type Attachment,
} from "./wire";

export function useChatSession() {
  const showToast = useToast();

  // Chat history hangs off a user row. Bootstrap-admin sessions have none, so
  // skip the history requests entirely instead of letting them 403.
  const me = useMe();
  const canPersist = Boolean(me.data?.user_id);

  const chats = useChats(canPersist);
  const models = useChatModels();

  // Pricing inputs for the per-turn cost estimate, and the context window for
  // the meter. Deployments are admin-only, so both may be empty for a
  // non-admin session — the UI degrades to hiding those readouts.
  const isAdmin = !!me.data?.is_admin;
  const deployments = useDeployments(isAdmin);
  const prices = usePrices(isAdmin);
  const createChat = useCreateChat();
  const updateChat = useUpdateChat();
  const deleteChat = useDeleteChat();

  const [activeId, setActiveId] = useState("");
  const active = useChat(activeId, canPersist);

  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [model, setModel] = useState("");
  const [systemPrompt, setSystemPrompt] = useState("");
  const [settings, setSettings] = useState<ChatSettings>(DEFAULT_SETTINGS);

  const [busy, setBusy] = useState(false);
  const [streamText, setStreamText] = useState("");
  const [streamReason, setStreamReason] = useState("");
  /**
   * Virtual API key to send as. Kept in sessionStorage, not localStorage: it is
   * a live credential and should not outlive the browser session.
   */
  const [sendAsKey, setSendAsKeyState] = useState(
    () => sessionStorage.getItem("llmr_chat_send_as") ?? "",
  );
  const setSendAsKey = useCallback((key: string) => {
    setSendAsKeyState(key);
    if (key) sessionStorage.setItem("llmr_chat_send_as", key);
    else sessionStorage.removeItem("llmr_chat_send_as");
  }, []);
  const [error, setError] = useState("");
  const [toolResults, setToolResults] = useState<Record<string, string>>({});
  // Set when a save is rejected at runtime; combined with the known-upfront
  // `!canPersist` case below so the UI only has one flag to reason about.
  const [saveFailed, setSaveFailed] = useState(false);
  const saveBlocked = saveFailed || !canPersist;

  const abortRef = useRef<AbortController | null>(null);
  const hydratedRef = useRef("");

  const priceMap = useMemo(
    () => new Map((prices.data ?? []).map((p) => [p.model_id, p])),
    [prices.data],
  );

  /** Context window for the active alias, when an operator has set one. */
  const contextTokens = useMemo(
    () =>
      (deployments.data ?? []).find((d) => d.model_name === model && d.enabled)?.context_tokens ??
      null,
    [deployments.data, model],
  );

  // Default to the first available model once discovery resolves.
  useEffect(() => {
    if (!model && models.data?.length) setModel(models.data[0]);
  }, [models.data, model]);

  // Hydrate local state when a saved conversation is opened.
  useEffect(() => {
    const d = active.data;
    if (!d || hydratedRef.current === d.id) return;
    hydratedRef.current = d.id;
    setMessages(d.messages ?? []);
    setModel(d.model || (models.data?.[0] ?? ""));
    setSystemPrompt(d.system_prompt ?? "");
    setSettings({ ...DEFAULT_SETTINGS, ...(d.settings ?? {}) });
    setError("");
    setToolResults({});
  }, [active.data, models.data]);

  // Never leave a stream running after the page goes away.
  useEffect(() => () => abortRef.current?.abort(), []);

  const reset = useCallback(() => {
    abortRef.current?.abort();
    setActiveId("");
    hydratedRef.current = "";
    setMessages([]);
    setSystemPrompt("");
    setSettings(DEFAULT_SETTINGS);
    setError("");
    setToolResults({});
  }, []);

  const open = useCallback((id: string) => {
    abortRef.current?.abort();
    hydratedRef.current = "";
    setActiveId(id);
  }, []);

  /** Persist the conversation, creating it on first save. Never blocks the UI. */
  const persist = useCallback(
    async (msgs: ChatMessage[]) => {
      if (saveBlocked) return;
      const body = {
        title: deriveTitle(msgs),
        model,
        system_prompt: systemPrompt,
        settings: settings as unknown as Record<string, unknown>,
        messages: msgs,
      };
      try {
        let id = activeId;
        if (!id) {
          // useInvalidatingMutation erases the return type — cast it back.
          id = ((await createChat.mutateAsync({ title: body.title, model })) as { id: string }).id;
          hydratedRef.current = id; // local state is already the source of truth
          setActiveId(id);
        }
        await updateChat.mutateAsync({ id, ...body });
      } catch (err) {
        if (!saveBlocked) {
          showToast({
            body: err instanceof Error ? err.message : "Chat not saved",
            type: "error",
            uniqueID: "chat-save",
          });
        }
        setSaveFailed(true);
      }
    },
    [activeId, createChat, model, saveBlocked, settings, showToast, systemPrompt, updateChat],
  );

  /** Run one completion turn over `msgs` and append the assistant reply. */
  const runTurn = useCallback(
    async (msgs: ChatMessage[]) => {
      if (!model) {
        showToast({ body: "Pick a model first", type: "error" });
        return;
      }
      const built = wireExtras(settings);
      if ("error" in built) {
        showToast({ body: built.error, type: "error" });
        return;
      }

      setBusy(true);
      setError("");
      setStreamText("");
      setStreamReason("");

      const ac = new AbortController();
      abortRef.current = ac;

      let text = "";
      let reasoning = "";
      const toolAcc: Record<number, ChatToolCall> = {};
      let usage: { prompt: number; completion: number; cached?: number } | undefined;

      const startedAt = performance.now();
      const turn: TurnMetrics = {};
      if (sendAsKey) turn.sentAsKey = sendAsKey.slice(0, 12);

      try {
        // The console streams through /api/chat/completions, which authenticates
        // with the operator's session. /v1/chat/completions is the client-facing
        // surface and requires a virtual API key the console does not hold.
        // Sending as a virtual key goes through the public client surface so the
        // key's own allowed models, budget and rate limits apply — that is the
        // whole point of the mode. Otherwise use the console endpoint, which
        // authenticates with the operator's session.
        await postStream(
          sendAsKey ? "/v1/chat/completions" : "/api/chat/completions",
          {
            model,
            messages: wireMessages(systemPrompt, msgs),
            stream: true,
            stream_options: { include_usage: true },
            ...built.extras,
          },
          {
            signal: ac.signal,
            authorization: sendAsKey ? `Bearer ${sendAsKey}` : undefined,
            onResponse: (res) => {
              turn.requestId = res.headers.get("X-Request-Id") ?? undefined;
              turn.provider = res.headers.get("X-Llmr-Provider") ?? undefined;
              turn.upstream = res.headers.get("X-Llmr-Upstream") ?? undefined;
              const attempts = Number(res.headers.get("X-Llmr-Attempts"));
              if (attempts > 0) turn.attempts = attempts;
            },
            onChunk: (c) => {
              const delta = c.choices?.[0]?.delta;
              if (delta?.content) {
                // First visible token: what the caller perceives as latency.
                turn.ttftMs ??= performance.now() - startedAt;
                text += delta.content;
                setStreamText(text);
              }
              if (delta?.reasoning_content) {
                reasoning += delta.reasoning_content;
                setStreamReason(reasoning);
              }
              // Tool calls arrive as indexed fragments that must be concatenated.
              for (const tc of delta?.tool_calls ?? []) {
                const cur = (toolAcc[tc.index] ??= {
                  id: "",
                  type: "function",
                  function: { name: "", arguments: "" },
                });
                if (tc.id) cur.id = tc.id;
                if (tc.function?.name) cur.function.name += tc.function.name;
                if (tc.function?.arguments) cur.function.arguments += tc.function.arguments;
              }
              if (c.usage) {
                usage = {
                  prompt: c.usage.prompt_tokens ?? 0,
                  completion: c.usage.completion_tokens ?? 0,
                };
              }
              if (delta?.reasoning_content) turn.ttftMs ??= performance.now() - startedAt;
            },
          },
        );

        const toolCalls = Object.keys(toolAcc)
          .sort((a, b) => Number(a) - Number(b))
          .map((k) => toolAcc[Number(k)]);

        turn.totalMs = performance.now() - startedAt;
        if (usage) {
          const est = estimateCost(
            (deployments.data ?? []).find((d) => d.model_name === model && d.enabled),
            priceMap,
            usage,
          );
          turn.costUsd = est.usd;
          turn.unpriced = est.unpriced;
        }

        const next: ChatMessage[] = [
          ...msgs,
          {
            role: "assistant",
            content: text || null,
            ...(toolCalls.length ? { tool_calls: toolCalls } : {}),
            ...(usage ? { _usage: usage } : {}),
            ...(reasoning ? { _reasoning: reasoning } : {}),
            _model: model,
            _turn: turn,
          },
        ];
        setMessages(next);
        void persist(next);
      } catch (err) {
        // Keep whatever streamed before the failure rather than losing it.
        if (text || reasoning) {
          turn.totalMs = performance.now() - startedAt;
          setMessages([
            ...msgs,
            {
              role: "assistant",
              content: text || null,
              ...(reasoning ? { _reasoning: reasoning } : {}),
              _model: model,
              _turn: turn,
            },
          ]);
        }
        setError(err instanceof Error ? err.message : "Request failed");
      } finally {
        setStreamText("");
        setStreamReason("");
        setBusy(false);
        abortRef.current = null;
      }
    },
    [deployments.data, model, persist, priceMap, sendAsKey, settings, showToast, systemPrompt],
  );

  const send = useCallback(
    async (draft: string, attachments: Attachment[]) => {
      const text = draft.trim();
      if ((!text && attachments.length === 0) || busy) return;
      const next: ChatMessage[] = [
        ...messages,
        { role: "user", content: buildUserContent(text, attachments) },
      ];
      setMessages(next);
      await runTurn(next);
    },
    [busy, messages, runTurn],
  );

  /** The tool loop: attach a typed result per pending call, then continue. */
  const sendToolResults = useCallback(
    async (calls: ChatToolCall[]) => {
      const next: ChatMessage[] = [
        ...messages,
        ...calls.map((c) => ({
          role: "tool" as const,
          tool_call_id: c.id,
          content: toolResults[c.id] ?? "",
        })),
      ];
      setMessages(next);
      setToolResults({});
      await runTurn(next);
    },
    [messages, runTurn, toolResults],
  );

  const remove = useCallback(
    async (id: string) => {
      try {
        await deleteChat.mutateAsync(id);
        if (id === activeId) reset();
        showToast({ body: "Chat deleted" });
      } catch (err) {
        showToast({ body: err instanceof Error ? err.message : "Failed", type: "error" });
      }
    },
    [activeId, deleteChat, reset, showToast],
  );

  const stop = useCallback(() => abortRef.current?.abort(), []);

  /**
   * Re-run the last assistant turn: drop it and everything after the preceding
   * user message, then send again. Uses whatever model is selected now, so this
   * doubles as "retry with a different model".
   */
  const regenerate = useCallback(async () => {
    if (busy) return;
    let cut = messages.length;
    while (cut > 0 && messages[cut - 1].role !== "user") cut--;
    if (cut === 0) return;
    const history = messages.slice(0, cut);
    setMessages(history);
    await runTurn(history);
  }, [busy, messages, runTurn]);

  /** Tokens currently in the conversation, for the context meter. */
  const usedTokens = useMemo(() => {
    // Prefer the server's own count from the most recent turn; it accounts for
    // system prompt, tool definitions and image tokens that a character-based
    // estimate cannot see.
    for (let i = messages.length - 1; i >= 0; i--) {
      const u = messages[i]._usage;
      if (u) return u.prompt + u.completion;
    }
    return 0;
  }, [messages]);

  /** Trailing assistant message with unanswered tool calls, if any. */
  const pendingCalls = useMemo(() => {
    const last = messages[messages.length - 1];
    return !busy && last?.role === "assistant" && last.tool_calls?.length ? last.tool_calls : null;
  }, [messages, busy]);

  return {
    // data
    chats,
    models,
    activeId,
    messages,
    model,
    systemPrompt,
    settings,
    busy,
    streamText,
    streamReason,
    error,
    toolResults,
    pendingCalls,
    saveBlocked,
    canPersist,
    saveFailed,
    isDeleting: deleteChat.isPending,
    sendAsKey,
    contextTokens,
    usedTokens,
    // actions
    setSendAsKey,
    regenerate,
    setModel,
    setSystemPrompt,
    setSettings,
    setToolResults,
    open,
    reset,
    send,
    sendToolResults,
    stop,
    remove,
  };
}

export type ChatSession = ReturnType<typeof useChatSession>;
