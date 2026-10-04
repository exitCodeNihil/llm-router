/**
 * Pure request-shaping helpers for the playground.
 *
 * Everything here is side-effect free and independently testable: given
 * settings + messages + attachments, produce the OpenAI wire payload. Keeping
 * this separate from React means the tricky parts (JSON validation, content
 * parts, client-only field stripping) can be reasoned about on their own.
 */
import type {
  ChatContentPart,
  ChatMessage,
  ChatSettings,
  ChatUITool,
} from "../../lib/types";

export const DEFAULT_SETTINGS: ChatSettings = {
  temperature: null,
  max_tokens: null,
  response_format: "none",
  json_schema: "",
  tools: [],
  tool_choice: "auto",
};

/** Chat rows are stored as JSON in Postgres — keep attachments small. */
export const MAX_IMAGE_BYTES = 2 * 1024 * 1024;
export const MAX_TEXT_BYTES = 200 * 1024;

export const TEXT_EXTS =
  /\.(txt|md|markdown|csv|json|jsonl|xml|ya?ml|toml|log|py|go|js|jsx|ts|tsx|rs|java|c|h|cpp|sql|sh|html|css)$/i;

export const FILE_ACCEPT =
  "image/*,text/*,.md,.csv,.json,.py,.go,.js,.ts,.tsx,.sql,.sh,.yaml,.yml,.log";

/**
 * Pasting more than this many characters becomes a text attachment instead of
 * filling the composer, matching ChatGPT and Claude. Keeps a 5,000-line log
 * paste from burying the input while still sending the whole thing.
 */
export const PASTE_AS_FILE_CHARS = 1500;

export type Attachment =
  | { kind: "image"; name: string; dataUrl: string }
  | { kind: "text"; name: string; text: string };

export const newTool = (): ChatUITool => ({
  name: "",
  description: "",
  parameters: '{"type":"object","properties":{}}',
});

/** True when the string is non-empty and not parseable as JSON. */
export function isInvalidJson(text: string): boolean {
  if (!text.trim()) return false;
  try {
    JSON.parse(text);
    return false;
  } catch {
    return true;
  }
}

export const schemaInvalid = (s: ChatSettings): boolean =>
  s.response_format === "json_schema" && (!s.json_schema.trim() || isInvalidJson(s.json_schema));

/**
 * Transcript in wire form: optional system prompt first, then every message
 * with client-only underscore fields (_usage, _reasoning, _error) stripped —
 * the upstream API rejects unknown properties.
 */
export function wireMessages(systemPrompt: string, msgs: ChatMessage[]): unknown[] {
  const wire: unknown[] = [];
  if (systemPrompt.trim()) wire.push({ role: "system", content: systemPrompt });
  for (const m of msgs) {
    wire.push(Object.fromEntries(Object.entries(m).filter(([k]) => !k.startsWith("_"))));
  }
  return wire;
}

export type WireExtras = Record<string, unknown>;

/**
 * Sampling / structured-output / tool parameters.
 *
 * Returns an error message instead of throwing so the caller can surface it in
 * a toast: invalid JSON is user input, not an exceptional condition.
 */
export function wireExtras(s: ChatSettings): { extras: WireExtras } | { error: string } {
  const extras: WireExtras = {};

  if (s.temperature != null) extras.temperature = s.temperature;
  if (s.max_tokens != null && s.max_tokens > 0) extras.max_tokens = s.max_tokens;

  if (s.response_format === "json_object") {
    extras.response_format = { type: "json_object" };
  } else if (s.response_format === "json_schema") {
    try {
      extras.response_format = {
        type: "json_schema",
        json_schema: { name: "response", strict: true, schema: JSON.parse(s.json_schema) },
      };
    } catch {
      return { error: "Structured output schema is not valid JSON" };
    }
  }

  const tools: unknown[] = [];
  for (const t of s.tools) {
    if (!t.name.trim()) continue;
    try {
      tools.push({
        type: "function",
        function: {
          name: t.name.trim(),
          description: t.description,
          parameters: t.parameters.trim()
            ? JSON.parse(t.parameters)
            : { type: "object", properties: {} },
        },
      });
    } catch {
      return { error: `Tool "${t.name}" parameters are not valid JSON` };
    }
  }
  if (tools.length) {
    extras.tools = tools;
    extras.tool_choice = s.tool_choice;
  }

  return { extras };
}

/**
 * Compose the user turn. Text files are inlined as fenced blocks so any model
 * can read them; images become content parts, which only vision models accept.
 */
export function buildUserContent(text: string, attachments: Attachment[]): ChatMessage["content"] {
  let full = text;
  for (const a of attachments) {
    if (a.kind === "text") full += `${full ? "\n\n" : ""}${a.name}:\n\`\`\`\n${a.text}\n\`\`\``;
  }

  const images = attachments.filter((a) => a.kind === "image");
  if (!images.length) return full;

  const parts: ChatContentPart[] = [];
  if (full) parts.push({ type: "text", text: full });
  for (const img of images) parts.push({ type: "image_url", image_url: { url: img.dataUrl } });
  return parts;
}

/**
 * First user line, used as the saved conversation's title.
 *
 * Whitespace is collapsed before truncating: attachments are inlined into the
 * message as fenced blocks, so the raw text is often multi-line and would
 * otherwise produce a title containing an entire file.
 */
export function deriveTitle(msgs: ChatMessage[]): string {
  const first = msgs.find((m) => m.role === "user" && typeof m.content === "string");
  const text = typeof first?.content === "string" ? first.content : "";
  const oneLine = text.replace(/\s+/g, " ").trim();
  return oneLine.slice(0, 60) || (msgs.length ? "Chat" : "New chat");
}

/** Single-line, length-capped label for the sidebar. */
export function displayTitle(title: string, max = 34): string {
  const oneLine = (title || "Untitled").replace(/\s+/g, " ").trim();
  return oneLine.length > max ? `${oneLine.slice(0, max - 1)}…` : oneLine;
}

/**
 * Turn a large clipboard paste into a text attachment. Numbered so several
 * pastes in one turn stay distinguishable in the transcript.
 */
export function pastedTextAttachment(text: string, index: number): Attachment {
  return { kind: "text", name: `pasted-${index}.txt`, text };
}

/** Read one picked file into an Attachment, or explain why it was rejected. */
export async function readAttachment(
  file: File,
): Promise<{ attachment: Attachment } | { error: string }> {
  if (file.type.startsWith("image/")) {
    if (file.size > MAX_IMAGE_BYTES) return { error: `${file.name}: images are capped at 2MB` };
    const dataUrl = await new Promise<string>((resolve, reject) => {
      const r = new FileReader();
      r.onload = () => resolve(r.result as string);
      r.onerror = () => reject(r.error);
      r.readAsDataURL(file);
    });
    return { attachment: { kind: "image", name: file.name, dataUrl } };
  }

  if (file.type.startsWith("text/") || TEXT_EXTS.test(file.name)) {
    if (file.size > MAX_TEXT_BYTES) return { error: `${file.name}: text files are capped at 200KB` };
    return { attachment: { kind: "text", name: file.name, text: await file.text() } };
  }

  return { error: `${file.name}: unsupported type (images and text files only)` };
}
