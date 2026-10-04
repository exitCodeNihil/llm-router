/**
 * Routing-flow diagram: KEYS → ALIASES → UPSTREAMS.
 *
 * Edge stroke width is proportional to volume, upstream chips carry a status
 * dot, and second-hop edges get an animated flow dot (which the global
 * prefers-reduced-motion rule pauses). Columns sit on a fixed 372-wide viewBox;
 * height scales with the tallest column. Labels are drawn as-is — truncate
 * before passing them in.
 *
 * Click a chip to trace everything it touches — its whole route through the
 * three columns — or a line to isolate that single hop. Everything else dims.
 * Click the background to clear. Chips are focusable so the same tracing works
 * from the keyboard; lines are mouse-only, since selecting either endpoint chip
 * reaches the same information without adding a tab stop per edge.
 *
 * Astryx has no Sankey primitive, so this stays hand-rolled SVG. It is
 * design-system agnostic: every colour is an Astryx token reference, so it
 * tracks light/dark and any future theme change automatically.
 */
import { useMemo, useState } from "react";

export type SankeyNode = { id: string; label: string; led?: "ok" | "warn" | "err" };
export type SankeyEdge = { from: string; to: string; volume: number; color?: string };

const W = 372;
const PITCH = 52;
const KEY_L = 0;
const KEY_R = 104;
const ALIAS_L = 140;
const ALIAS_R = 232;
const UP_L = 268;
const UP_R = 372;

type Placed = SankeyNode & { leftX: number; rightX: number; cy: number };

const ledColor = (led?: "ok" | "warn" | "err") =>
  led === "warn"
    ? "var(--color-warning)"
    : led === "err"
      ? "var(--color-error)"
      : "var(--color-success)";

const FLOW_CSS = `
@keyframes sankey-flow { from { offset-distance: 0%; } to { offset-distance: 100%; } }
.sankey-flow-dot { offset-distance: 0%; animation: sankey-flow 3.2s linear infinite; }
@media (prefers-reduced-motion: reduce) { .sankey-flow-dot { animation: none; } }
`;

export function Sankey({
  keys,
  aliases,
  upstreams,
  edges,
}: {
  keys: SankeyNode[];
  aliases: SankeyNode[];
  upstreams: SankeyNode[];
  edges: SankeyEdge[];
}) {
  const rows = Math.max(keys.length, aliases.length, upstreams.length, 1);
  const H = rows * PITCH;

  const place = (list: SankeyNode[], leftX: number, rightX: number): Placed[] => {
    const n = Math.max(list.length, 1);
    return list.map((node, i) => ({ ...node, leftX, rightX, cy: (H * (i + 0.5)) / n }));
  };

  const kNodes = place(keys, KEY_L, KEY_R);
  const aNodes = place(aliases, ALIAS_L, ALIAS_R);
  const uNodes = place(upstreams, UP_L, UP_R);

  const byId = new Map<string, Placed>();
  for (const n of [...kNodes, ...aNodes, ...uNodes]) byId.set(n.id, n);
  const aliasIds = new Set(aNodes.map((n) => n.id));
  const upstreamIds = new Set(uNodes.map((n) => n.id));

  const maxVol = Math.max(1, ...edges.map((e) => e.volume));

  // Selection is by value, not array index: the flow data refetches on a timer
  // and a reordered edge list would otherwise move the highlight.
  const [sel, setSel] = useState<{ kind: "node"; id: string } | { kind: "edge"; id: string } | null>(
    null,
  );
  const edgeID = (e: SankeyEdge) => `${e.from}|${e.to}`;

  const { activeNodes, activeEdges } = useMemo(() => {
    if (!sel) return { activeNodes: null as Set<string> | null, activeEdges: null as Set<string> | null };
    if (sel.kind === "edge") {
      const e = edges.find((x) => edgeID(x) === sel.id);
      return e
        ? { activeNodes: new Set([e.from, e.to]), activeEdges: new Set([sel.id]) }
        : { activeNodes: null, activeEdges: null };
    }
    // Strictly one direction per pass. Walking both from every node reached
    // would flood the whole connected component: from an alias it would climb to
    // its keys, then descend again into every *other* alias those keys use.
    const grow = (seed: string, step: (e: SankeyEdge, s: Set<string>) => void) => {
      const s = new Set([seed]);
      for (let n = -1; n !== s.size; ) {
        n = s.size;
        for (const e of edges) step(e, s);
      }
      return s;
    };
    const anc = grow(sel.id, (e, s) => {
      if (s.has(e.to)) s.add(e.from);
    });
    const desc = grow(sel.id, (e, s) => {
      if (s.has(e.from)) s.add(e.to);
    });
    const eids = new Set(
      edges
        .filter(
          (e) => (anc.has(e.from) && anc.has(e.to)) || (desc.has(e.from) && desc.has(e.to)),
        )
        .map(edgeID),
    );
    return { activeNodes: new Set([...anc, ...desc]), activeEdges: eids };
  }, [sel, edges]);

  const nodeDim = (id: string) => (activeNodes && !activeNodes.has(id) ? 0.2 : 1);
  const edgeOpacity = (id: string) => {
    if (!activeEdges) return 0.6;
    return activeEdges.has(id) ? 0.95 : 0.08;
  };
  const toggle = (s: { kind: "node" | "edge"; id: string }) =>
    setSel((cur) => (cur && cur.kind === s.kind && cur.id === s.id ? null : s));

  const path = (a: Placed, b: Placed) => {
    const mx = (a.rightX + b.leftX) / 2;
    return `M${a.rightX},${a.cy} C${mx},${a.cy} ${mx},${b.cy} ${b.leftX},${b.cy}`;
  };

  const chip = (n: Placed, w: number, isUpstream = false) => {
    const picked = sel?.kind === "node" && sel.id === n.id;
    return (
    <g
      key={n.id}
      role="button"
      tabIndex={0}
      aria-pressed={picked}
      aria-label={`${n.label} — trace connections`}
      style={{ cursor: "pointer", opacity: nodeDim(n.id) }}
      onClick={() => toggle({ kind: "node", id: n.id })}
      onKeyDown={(ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          toggle({ kind: "node", id: n.id });
        }
      }}
    >
      <rect
        x={n.leftX}
        y={n.cy - 12}
        width={w}
        height={24}
        rx={6}
        fill={picked ? "var(--color-accent-muted)" : "var(--color-background-muted)"}
        stroke={picked ? "var(--color-accent)" : "var(--color-border)"}
        strokeWidth={picked ? 1.5 : 1}
      />
      {isUpstream && <circle cx={n.leftX + 11} cy={n.cy} r={3} fill={ledColor(n.led)} />}
      <text
        x={isUpstream ? n.leftX + 20 : n.leftX + 8}
        y={n.cy + 4}
        fill="var(--color-text-primary)"
        fontSize={10}
        fontFamily="var(--font-family-code)"
      >
        {n.label}
      </text>
    </g>
    );
  };

  return (
    <div style={{ position: "relative", width: "100%", aspectRatio: `${W} / ${H}` }}>
      <style>{FLOW_CSS}</style>
      <svg
        style={{ position: "absolute", inset: 0 }}
        width="100%"
        height="100%"
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="xMidYMid meet"
        role="group"
        aria-label={`Routing flow: ${keys.length} keys through ${aliases.length} model aliases to ${upstreams.length} upstreams. Select a node to trace its connections.`}
      >
        {/* Clears the selection; behind everything so chips and lines win. */}
        <rect width={W} height={H} fill="transparent" onClick={() => setSel(null)} />
        {edges.map((e, i) => {
          const a = byId.get(e.from);
          const b = byId.get(e.to);
          if (!a || !b) return null;
          const d = path(a, b);
          const color = e.color ?? "var(--color-text-disabled)";
          const isSecondHop = aliasIds.has(e.from) && upstreamIds.has(e.to);
          const id = edgeID(e);
          const width = 1 + (e.volume / maxVol) * 5;
          return (
            <g key={`e${i}`}>
              {/* Fat invisible copy: a 1px hairline is unclickable in practice. */}
              <path
                d={d}
                stroke="transparent"
                strokeWidth={Math.max(width, 10)}
                fill="none"
                style={{ cursor: "pointer" }}
                onClick={() => toggle({ kind: "edge", id })}
              />
              <path
                d={d}
                stroke={color}
                strokeWidth={width}
                fill="none"
                opacity={edgeOpacity(id)}
                pointerEvents="none"
              />
              {isSecondHop && edgeOpacity(id) > 0.2 && (
                <rect
                  width={4}
                  height={4}
                  rx={2}
                  fill={color}
                  className="sankey-flow-dot"
                  style={{ offsetPath: `path('${d}')` }}
                />
              )}
            </g>
          );
        })}
        {kNodes.map((n) => chip(n, KEY_R - KEY_L))}
        {aNodes.map((n) => chip(n, ALIAS_R - ALIAS_L))}
        {uNodes.map((n) => chip(n, UP_R - UP_L, true))}
      </svg>
    </div>
  );
}
