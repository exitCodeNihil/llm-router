/**
 * llm-router theme — the single source of visual truth for the console.
 *
 * The product's design language is a telemetry aesthetic: cool slate base, one
 * amber "signal" accent, monospace for data. Everything below maps that onto
 * Astryx tokens, so restyling the console means editing this file and nothing
 * else. Values are [light, dark] tuples, which defineTheme compiles to CSS
 * light-dark().
 *
 * After editing, run `make theme` (or `npm run theme`) to recompile
 * theme.built.css — the build does this automatically.
 */
import { defineTheme } from "@astryxdesign/core/theme";
import { neutralTheme } from "@astryxdesign/theme-neutral";

// ── Palette ────────────────────────────────────────────
const bg = ["#f6f7f9", "#090b10"] as const;
const surface = ["#ffffff", "#10141c"] as const;
const surface2 = ["#f0f2f5", "#171d28"] as const;
const border = ["#e4e7ec", "#212838"] as const;
const borderStrong = ["#d2d7de", "#2d3648"] as const;
const ink = ["#171b23", "#e7eaf0"] as const;
const ink2 = ["#58606f", "#98a0b2"] as const;
const ink3 = ["#8a91a0", "#5b6376"] as const;
const signal = ["#b06d00", "#f5b84b"] as const;
const signalSoft = ["rgba(176, 109, 0, 0.10)", "rgba(245, 184, 75, 0.12)"] as const;
const good = ["#1a7f37", "#3fb950"] as const;
const warn = ["#9a6700", "#d9a22e"] as const;
const crit = ["#cf372b", "#f0603a"] as const;
const info = ["#1f6feb", "#4a9eed"] as const;

const mono = 'ui-monospace, "SF Mono", "JetBrains Mono", "Fira Code", Menlo, Consolas, monospace';
const sans =
  'ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif';

export const llmRouterTheme = defineTheme({
  name: "llm-router",
  extends: neutralTheme,

  // 14px base with a tight ratio — dense telemetry UI, not a marketing site.
  typography: {
    scale: { base: 14, ratio: 1.2 },
    body: { family: "", fallbacks: sans },
    heading: { family: "", fallbacks: sans, weight: "semibold" },
    code: { family: "", fallbacks: mono },
  },

  // 0.5rem containers, matching the console's long-standing surface radius.
  radius: { base: 4, multiplier: 1 },

  // The generated --radius-chat is 28px, which renders a two-character message
  // as a circle that reads as an avatar. 14px keeps bubbles friendly without
  // going pill-shaped, and suits the console's boxier telemetry aesthetic.

  tokens: {
    // Accent — the amber signal wire.
    "--color-accent": [...signal],
    "--color-accent-muted": [...signalSoft],
    "--color-text-accent": [...signal],
    "--color-icon-accent": [...signal],
    // Amber is a light hue: dark ink on accent fills reads far better than white.
    "--color-on-accent": ["#ffffff", "#101010"],

    // Surfaces.
    "--color-background-body": [...bg],
    "--color-background-surface": [...surface],
    "--color-background-card": [...surface],
    "--color-background-popover": [...surface],
    "--color-background-muted": [...surface2],

    // Text + icons.
    "--color-text-primary": [...ink],
    "--color-text-secondary": [...ink2],
    "--color-text-disabled": [...ink3],
    "--color-icon-primary": [...ink],
    "--color-icon-secondary": [...ink2],
    "--color-icon-disabled": [...ink3],

    "--radius-chat": "14px",

    // Lines.
    "--color-border": [...border],
    "--color-border-emphasized": [...borderStrong],
    "--color-skeleton": [...surface2],
    "--color-track": [...surface2],

    // Status — good / warn / crit map to the existing semantic trio.
    "--color-success": [...good],
    "--color-warning": [...warn],
    "--color-error": [...crit],
    "--color-text-blue": [...info],
    "--color-icon-blue": [...info],
    "--color-border-blue": [...info],
  },

  components: {
    // Tables carry numbers everywhere; tabular figures stop columns from
    // jittering as live data refreshes. This is the console's signature
    // "data is monospaced" treatment, applied at the theme layer.
    table: { base: { fontVariantNumeric: "tabular-nums" } },
    kbd: { base: { fontFamily: mono } },
  },
});

/** Persisted colour mode. Storage key predates the Astryx port; kept stable. */
export type Mode = "light" | "dark";
const MODE_KEY = "llmr_theme";

export function readMode(): Mode {
  return localStorage.getItem(MODE_KEY) === "light" ? "light" : "dark";
}
export function writeMode(m: Mode) {
  localStorage.setItem(MODE_KEY, m);
}
