// Streaming POST for the chat playground: parses OpenAI-style SSE
// (`data: {...}` lines, `data: [DONE]` terminator) from a fetch body stream.
// Auth mirrors api.ts: optional bearer token + same-origin cookie.

import { token } from "./api";

/** One parsed chunk of /v1/chat/completions SSE. */
export interface ChatChunk {
  choices?: {
    index?: number;
    delta?: {
      role?: string;
      content?: string;
      /** Reasoning-model channel (DeepSeek-R1 style, LM Studio, etc.). */
      reasoning_content?: string;
      tool_calls?: {
        index: number;
        id?: string;
        type?: string;
        function?: { name?: string; arguments?: string };
      }[];
    };
    finish_reason?: string | null;
  }[];
  usage?: { prompt_tokens?: number; completion_tokens?: number } | null;
}

export interface StreamHandlers {
  onChunk: (c: ChatChunk) => void;
  /**
   * Called once with the response before the body is consumed, so callers can
   * read routing headers (X-Request-Id, X-Llmr-Provider, X-Llmr-Upstream,
   * X-Llmr-Attempts) that the gateway sets before streaming starts.
   */
  onResponse?: (res: Response) => void;
  /**
   * Bearer token to use instead of the console session. Set this to send as a
   * virtual API key, which exercises the real client path including that key's
   * allowed models, budget and rate limits.
   */
  authorization?: string;
  signal?: AbortSignal;
}

/**
 * One line of a workspace terminal or chat stream. `exit` carries a non-zero
 * exit code; `error` is the gateway failing rather than the command.
 */
export interface WorkspaceLine {
  stream: "stdout" | "stderr" | "exit" | "error";
  data: string;
}

/**
 * POST body to path and consume a newline-delimited JSON response, calling
 * onLine per complete line. Workspace exec and chat stream ndjson rather than
 * SSE — same reader machinery, no `data:` framing.
 *
 * Resolves at EOF, throws on non-2xx, and an abort resolves silently.
 */
export async function postNDJSON(
  path: string,
  body: unknown,
  onLine: (line: WorkspaceLine) => void,
  signal?: AbortSignal,
): Promise<void> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const t = token.get();
  if (t) headers["Authorization"] = `Bearer ${t}`;

  const res = await fetch(path, {
    method: "POST",
    headers,
    credentials: "same-origin",
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok || !res.body) throw new Error(await errorMessage(res));

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      for (;;) {
        const nl = buf.indexOf("\n");
        if (nl < 0) break;
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try {
          onLine(JSON.parse(line) as WorkspaceLine);
        } catch {
          // A partial line can only happen at EOF; skip it.
        }
      }
    }
  } catch (err) {
    if (signal?.aborted) return;
    throw err;
  }
}

/** Unwrap the server's {"error": "..."} shape, falling back to raw text. */
async function errorMessage(res: Response): Promise<string> {
  const text = await res.text().catch(() => "");
  try {
    const j = JSON.parse(text);
    return j?.error?.message || j?.error || `Request failed (${res.status})`;
  } catch {
    return text ? text.slice(0, 300) : `Request failed (${res.status})`;
  }
}

/**
 * POST body to path and consume the SSE response. Resolves when the stream
 * ends ([DONE] or EOF). Throws on non-2xx (with the server's error message)
 * and on network failure; an abort resolves silently.
 */
export async function postStream(
  path: string,
  body: unknown,
  { onChunk, onResponse, authorization, signal }: StreamHandlers,
): Promise<void> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (authorization) {
    // Explicit credential: do not also send the console token, or the gateway
    // would authenticate as the operator instead of the key under test.
    headers["Authorization"] = authorization;
  } else {
    const t = token.get();
    if (t) headers["Authorization"] = `Bearer ${t}`;
  }

  const res = await fetch(path, {
    method: "POST",
    headers,
    // A virtual key must stand on its own; sending the session cookie too
    // would let the server fall back to the operator's identity.
    credentials: authorization ? "omit" : "same-origin",
    body: JSON.stringify(body),
    signal,
  });
  onResponse?.(res);

  if (!res.ok || !res.body) {
    const text = await res.text().catch(() => "");
    let msg = `Request failed (${res.status})`;
    try {
      const j = JSON.parse(text);
      msg = j?.error?.message || j?.error || msg;
    } catch {
      if (text) msg = text.slice(0, 300);
    }
    throw new Error(msg);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      // SSE events are newline-delimited; process complete lines only.
      for (;;) {
        const nl = buf.indexOf("\n");
        if (nl < 0) break;
        const line = buf.slice(0, nl).replace(/\r$/, "");
        buf = buf.slice(nl + 1);
        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (data === "[DONE]") return;
        try {
          onChunk(JSON.parse(data) as ChatChunk);
        } catch {
          // partial or non-JSON data line — skip
        }
      }
    }
  } catch (err) {
    if (signal?.aborted) return; // user hit Stop
    throw err;
  }
}
