// Shared API client. All calls are same-origin; auth is either a bootstrap
// token in localStorage (Bearer header) or an HttpOnly SSO cookie (sent
// automatically). Postgres numerics can arrive as strings, so every numeric
// field is coerced with num() at the typed-fetch boundary.

const TOKEN_KEY = "llmr_token";

export const token = {
  get: () => localStorage.getItem(TOKEN_KEY),
  set: (t: string) => localStorage.setItem(TOKEN_KEY, t),
  clear: () => localStorage.removeItem(TOKEN_KEY),
};

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {};
  const t = token.get();
  if (t) headers["Authorization"] = `Bearer ${t}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";

  const res = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (res.status === 204) return undefined as T;

  let payload: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = text;
    }
  }

  if (!res.ok) {
    const msg =
      (payload && typeof payload === "object" && "error" in payload
        ? String((payload as { error: unknown }).error)
        : typeof payload === "string"
          ? payload
          : "") || `Request failed (${res.status})`;
    throw new ApiError(res.status, msg);
  }
  return payload as T;
}

/** Multipart upload; the browser sets the boundary, so no Content-Type here. */
export async function upload<T>(method: string, path: string, form: FormData): Promise<T> {
  const headers: Record<string, string> = {};
  const t = token.get();
  if (t) headers["Authorization"] = `Bearer ${t}`;
  const res = await fetch(path, { method, headers, credentials: "same-origin", body: form });
  const text = await res.text();
  if (!res.ok) {
    let msg = text;
    try {
      msg = String((JSON.parse(text) as { error?: string }).error ?? text);
    } catch {
      /* plain text */
    }
    throw new ApiError(res.status, msg || `Request failed (${res.status})`);
  }
  return (text ? JSON.parse(text) : undefined) as T;
}

export const api = {
  get: <T>(p: string) => request<T>("GET", p),
  post: <T>(p: string, b?: unknown) => request<T>("POST", p, b ?? {}),
  patch: <T>(p: string, b: unknown) => request<T>("PATCH", p, b),
  put: <T>(p: string, b: unknown) => request<T>("PUT", p, b),
  del: <T>(p: string) => request<T>("DELETE", p),
};

/** Coerce Postgres string-numerics (and null) to a number. */
export function num(v: unknown): number {
  if (v === null || v === undefined || v === "") return 0;
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}
