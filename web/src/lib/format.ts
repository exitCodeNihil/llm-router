export function money(v: number, opts?: { precise?: boolean }): string {
  // Sub-millicent amounts (one Gemini Flash call) round to "$0.0000" at four
  // places, which reads as free; two significant digits keeps them honest.
  if (v > 0 && v < 0.001) {
    return "$" + v.toLocaleString("en-US", { maximumSignificantDigits: 2 });
  }
  if (v > 0 && (opts?.precise || v < 0.01)) {
    return "$" + v.toLocaleString("en-US", { minimumFractionDigits: 4, maximumFractionDigits: 4 });
  }
  return "$" + v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

export function plural(n: number, word: string): string {
  return `${int(n)} ${word}${n === 1 ? "" : "s"}`;
}

export function compact(v: number): string {
  return v.toLocaleString("en-US", { notation: "compact", maximumFractionDigits: 1 });
}

export function int(v: number): string {
  return Math.round(v).toLocaleString("en-US");
}

export function dateShort(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

export function dateTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function relTime(iso: string): string {
  const d = new Date(iso).getTime();
  if (isNaN(d)) return "—";
  const secs = Math.round((Date.now() - d) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  return dateShort(iso);
}

export function budgetLabel(v: number | null | undefined, period: string | null | undefined): string {
  if (v == null) return "Unlimited";
  return `${money(v)} / ${period ?? "total"}`;
}
