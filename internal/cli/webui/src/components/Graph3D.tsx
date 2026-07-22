// Graph3D renders a node-link graph in 3D on a plain 2D canvas — no WebGL / no
// three.js dependency. It reuses the same Graph data model as the 2D graph view
// (pivot.ts nodesEdges / nodesEdgesFromPath) and the shared node colouring
// (graphColors.ts), then runs a small 3D force-directed layout (Fruchterman–
// Reingold: all-pairs repulsion + per-edge attraction + mild gravity, cooling
// over time) and projects it with yaw/pitch rotation + perspective.
//
// Interaction: drag to rotate, scroll to zoom, hover a node for its source row
// (drawn on the canvas — there is no DOM element per node), click a node to
// drill into its subtree (same focus model as the 2D view), plus an
// auto-rotate toggle. The
// render loop lives in a mount-only effect and reads a mutable state ref, so
// unrelated re-renders (typing in the editor) only patch colours/labels — they
// never restart the layout or blank the canvas. The layout re-initialises only
// when the graph's structure (node/link/level/colour/size selection or focus)
// changes, tracked by a signature string.
import { memo, useEffect, useMemo, useRef, useState } from "react";
import type { Cell, Column } from "../api";
import { nodesEdges, nodesEdgesFromPath, rowLines } from "../pivot";
import { bubbleRadii, computeNodeColors, GraphColorLegend } from "../graphColors";

const NODE_CAP = 300; // nodes plotted before capping (matches the 2D view)
const LABEL_CAP = 60; // above this many nodes, labels are hidden (hover only)

const EDGE_COLOR = "138,147,167"; // rgb of the muted line colour

interface P3 {
  x: number;
  y: number;
  z: number;
}

interface Proj {
  sx: number;
  sy: number;
  r: number;
  ps: number; // perspective factor (depth cue: 1 = nearest)
  i: number;
}

// Mutable per-instance render state, read by the rAF loop and patched on render.
interface RenderState {
  pos: P3[];
  sig: string;
  n: number;
  edges: { source: number; target: number }[];
  colors: string[];
  labels: string[];
  keys: string[];
  sizes: (number | null)[];
  radii: number[];
  rootIndex: number;
  showLabels: boolean;
  colorName: string;
  onFocus: (key: string | null) => void;
  // hover tooltip: the source row behind each node, rendered on the canvas
  rows: Cell[][];
  columns: Column[];
  nodeRows: (number | null)[];
  rowDetail: boolean;
  hoverX: number | null; // pointer position in CSS px, null when off-canvas
  hoverY: number | null;
  hoverIdx: number; // node under the pointer this frame (-1 = none)
  tipIdx: number; // node the cached tip lines belong to
  tipLines: string[];
  // camera
  yaw: number;
  pitch: number;
  zoom: number;
  auto: boolean;
  // layout temperature
  alpha: number;
  // interaction bookkeeping
  dragging: boolean;
  moved: boolean;
  lastX: number;
  lastY: number;
  // viewport (CSS px)
  w: number;
  h: number;
  dpr: number;
  proj: Proj[];
}

// fibonacci sphere: a deterministic, well-spread initial placement.
function fibSphere(i: number, n: number): P3 {
  const phi = Math.acos(1 - (2 * (i + 0.5)) / n);
  const theta = Math.PI * (1 + Math.sqrt(5)) * i;
  return {
    x: Math.sin(phi) * Math.cos(theta),
    y: Math.sin(phi) * Math.sin(theta),
    z: Math.cos(phi),
  };
}

// stepForces advances the layout one iteration (Fruchterman–Reingold in 3D).
// Positions are in arbitrary units; the projection auto-fits to the canvas, so
// only the *relative* layout matters. Temperature (t) caps per-node motion and
// cools each frame.
function stepForces(pos: P3[], edges: { source: number; target: number }[], t: number) {
  const n = pos.length;
  const k = 1; // ideal edge length
  const disp: P3[] = new Array(n);
  for (let i = 0; i < n; i++) disp[i] = { x: 0, y: 0, z: 0 };

  // Repulsion between every pair.
  for (let i = 0; i < n; i++) {
    const pi = pos[i];
    for (let j = i + 1; j < n; j++) {
      const pj = pos[j];
      let dx = pi.x - pj.x;
      let dy = pi.y - pj.y;
      let dz = pi.z - pj.z;
      let d2 = dx * dx + dy * dy + dz * dz;
      if (d2 < 1e-4) {
        // Jitter coincident nodes apart deterministically.
        dx = (((i * 31 + j) % 7) - 3) * 1e-2;
        dy = (((i * 17 + j) % 5) - 2) * 1e-2;
        dz = (((i * 13 + j) % 3) - 1) * 1e-2;
        d2 = dx * dx + dy * dy + dz * dz + 1e-4;
      }
      const d = Math.sqrt(d2);
      const f = (k * k) / d; // repulsive
      const ux = dx / d;
      const uy = dy / d;
      const uz = dz / d;
      disp[i].x += ux * f;
      disp[i].y += uy * f;
      disp[i].z += uz * f;
      disp[j].x -= ux * f;
      disp[j].y -= uy * f;
      disp[j].z -= uz * f;
    }
  }
  // Attraction along edges.
  for (const e of edges) {
    const a = pos[e.source];
    const b = pos[e.target];
    if (!a || !b) continue;
    const dx = a.x - b.x;
    const dy = a.y - b.y;
    const dz = a.z - b.z;
    const d = Math.sqrt(dx * dx + dy * dy + dz * dz) || 1e-3;
    const f = (d * d) / k; // attractive
    const ux = (dx / d) * f;
    const uy = (dy / d) * f;
    const uz = (dz / d) * f;
    disp[e.source].x -= ux;
    disp[e.source].y -= uy;
    disp[e.source].z -= uz;
    disp[e.target].x += ux;
    disp[e.target].y += uy;
    disp[e.target].z += uz;
  }
  // Integrate: limit each node's step to the temperature; mild gravity keeps
  // disconnected components from drifting apart forever.
  const grav = 0.03;
  for (let i = 0; i < n; i++) {
    const p = pos[i];
    const d = disp[i];
    d.x -= p.x * grav;
    d.y -= p.y * grav;
    d.z -= p.z * grav;
    const len = Math.sqrt(d.x * d.x + d.y * d.y + d.z * d.z) || 1e-6;
    const lim = Math.min(len, t);
    p.x += (d.x / len) * lim;
    p.y += (d.y / len) * lim;
    p.z += (d.z / len) * lim;
  }
}

// hitTest returns the index of the node under (px, py) in canvas CSS px, or -1.
// Ties go to the nearest node (largest perspective factor), matching what the
// painter's-order draw put on top. The synthetic root (r=0) never hits.
function hitTest(proj: Proj[], px: number, py: number): number {
  let best = -1;
  let bestPs = -Infinity;
  for (const p of proj) {
    if (p.r <= 0) continue;
    const dx = p.sx - px;
    const dy = p.sy - py;
    if (dx * dx + dy * dy <= (p.r + 4) * (p.r + 4) && p.ps > bestPs) {
      best = p.i;
      bestPs = p.ps;
    }
  }
  return best;
}

export const Graph3D = memo(function Graph3D({
  canvasRef,
  rows,
  columns,
  numeric,
  graphSource,
  levels,
  nodeCol,
  linkCol,
  labelCol,
  sizeCol,
  colorCol,
  rowDetail,
  focusNode,
  onFocus,
}: {
  canvasRef: React.RefObject<HTMLDivElement>;
  rows: Cell[][];
  columns: Column[];
  numeric: { c: Column; i: number }[];
  graphSource: "edges" | "path";
  levels: number[];
  nodeCol: number;
  linkCol: number;
  labelCol: number;
  sizeCol: number;
  colorCol: number;
  rowDetail: boolean;
  focusNode: string | null;
  onFocus: (key: string | null) => void;
}) {
  const canvasEl = useRef<HTMLCanvasElement>(null);
  const stateRef = useRef<RenderState | null>(null);
  const [auto, setAuto] = useState(true);

  // Build the graph (mode is always "graph" — 3D is force-directed, not layered).
  const g = useMemo(
    () =>
      graphSource === "path"
        ? nodesEdgesFromPath(rows, levels, sizeCol, colorCol, "graph", focusNode, NODE_CAP)
        : nodesEdges(rows, nodeCol, linkCol, labelCol, sizeCol, colorCol, "graph", focusNode, NODE_CAP),
    [rows, graphSource, levels, nodeCol, linkCol, labelCol, sizeCol, colorCol, focusNode],
  );
  const numIdx = numeric.map((n) => n.i);
  const { colors, legend } = computeNodeColors(g, columns, colorCol, numIdx);
  const radii = bubbleRadii(g.nodes.map((n) => n.size));
  const showLabels = g.nodes.length <= LABEL_CAP;
  const colorName = colorCol >= 0 ? (columns[colorCol]?.name ?? "") : "";

  // Structural signature: when it changes, re-seed positions and re-heat.
  const sig = `${g.nodes.length}:${g.edges.length}:${graphSource}:${nodeCol}:${linkCol}:${sizeCol}:${colorCol}:${levels.join(",")}:${focusNode ?? ""}`;

  // Patch (or initialise) the render state every render. Mutating a ref here is
  // deliberate — it is not React state; the rAF loop reads the latest values.
  let st = stateRef.current;
  if (!st || st.sig !== sig) {
    const n = g.nodes.length;
    const r0 = Math.max(1, Math.cbrt(n) * 1.5);
    const pos = g.nodes.map((_, i) => {
      const p = fibSphere(i, Math.max(1, n));
      return { x: p.x * r0, y: p.y * r0, z: p.z * r0 };
    });
    st = {
      pos,
      sig,
      n,
      edges: g.edges,
      colors,
      labels: g.nodes.map((nd) => nd.label),
      keys: g.nodes.map((nd) => nd.key),
      sizes: g.nodes.map((nd) => nd.size),
      radii,
      rootIndex: g.rootIndex,
      showLabels,
      colorName,
      onFocus,
      rows,
      columns,
      nodeRows: g.nodes.map((nd) => nd.row),
      rowDetail,
      hoverX: st?.hoverX ?? null,
      hoverY: st?.hoverY ?? null,
      hoverIdx: -1,
      tipIdx: -1,
      tipLines: [],
      yaw: st?.yaw ?? 0.6,
      pitch: st?.pitch ?? -0.35,
      zoom: st?.zoom ?? 1,
      auto,
      alpha: 1,
      dragging: false,
      moved: false,
      lastX: 0,
      lastY: 0,
      w: st?.w ?? 300,
      h: st?.h ?? 300,
      dpr: st?.dpr ?? 1,
      proj: [],
    };
    stateRef.current = st;
  } else {
    // Same structure — just refresh the visual arrays + callbacks.
    st.edges = g.edges;
    st.colors = colors;
    st.labels = g.nodes.map((nd) => nd.label);
    st.keys = g.nodes.map((nd) => nd.key);
    st.sizes = g.nodes.map((nd) => nd.size);
    st.radii = radii;
    st.rootIndex = g.rootIndex;
    st.showLabels = showLabels;
    st.colorName = colorName;
    st.onFocus = onFocus;
    st.rows = rows;
    st.columns = columns;
    st.nodeRows = g.nodes.map((nd) => nd.row);
    st.rowDetail = rowDetail;
    st.tipIdx = -1; // the cached lines may describe stale data
  }
  st.auto = auto;

  // Mount-only: canvas sizing, input listeners, and the render loop.
  useEffect(() => {
    const canvas = canvasEl.current;
    const host = canvasRef.current;
    if (!canvas || !host) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    const ro = new ResizeObserver(() => {
      const s = stateRef.current;
      if (!s) return;
      const rect = host.getBoundingClientRect();
      s.w = Math.max(50, rect.width);
      s.h = Math.max(50, rect.height);
      s.dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(s.w * s.dpr);
      canvas.height = Math.round(s.h * s.dpr);
      canvas.style.width = `${s.w}px`;
      canvas.style.height = `${s.h}px`;
    });
    ro.observe(host);

    let raf = 0;
    const frame = () => {
      const s = stateRef.current;
      if (s) draw(ctx, s);
      raf = requestAnimationFrame(frame);
    };
    raf = requestAnimationFrame(frame);

    // ---- input ----
    const onDown = (e: PointerEvent) => {
      const s = stateRef.current;
      if (!s) return;
      s.dragging = true;
      s.moved = false;
      s.lastX = e.clientX;
      s.lastY = e.clientY;
      canvas.setPointerCapture(e.pointerId);
    };
    const onMove = (e: PointerEvent) => {
      const s = stateRef.current;
      if (!s) return;
      // Track the pointer for hover detail — the hit test itself happens in the
      // draw loop, against that frame's projection (the layout keeps moving).
      const rect = canvas.getBoundingClientRect();
      s.hoverX = e.clientX - rect.left;
      s.hoverY = e.clientY - rect.top;
      if (!s.dragging) return;
      const dx = e.clientX - s.lastX;
      const dy = e.clientY - s.lastY;
      if (Math.abs(dx) + Math.abs(dy) > 3) s.moved = true;
      s.yaw += dx * 0.01;
      s.pitch += dy * 0.01;
      s.pitch = Math.max(-1.45, Math.min(1.45, s.pitch));
      s.lastX = e.clientX;
      s.lastY = e.clientY;
    };
    const onUp = (e: PointerEvent) => {
      const s = stateRef.current;
      if (!s) return;
      s.dragging = false;
      if (!s.moved) {
        // A click (no drag): focus the nearest node under the pointer.
        const rect = canvas.getBoundingClientRect();
        const best = hitTest(s.proj, e.clientX - rect.left, e.clientY - rect.top);
        if (best >= 0 && best !== s.rootIndex) {
          const key = s.keys[best];
          if (key) s.onFocus(key);
        }
      }
      try {
        canvas.releasePointerCapture(e.pointerId);
      } catch {
        /* capture may already be gone */
      }
    };
    const onWheel = (e: WheelEvent) => {
      const s = stateRef.current;
      if (!s) return;
      e.preventDefault();
      s.zoom = Math.max(0.2, Math.min(6, s.zoom * Math.exp(-e.deltaY * 0.001)));
    };
    const onLeave = () => {
      const s = stateRef.current;
      if (!s) return;
      s.hoverX = null;
      s.hoverY = null;
    };
    canvas.addEventListener("pointerdown", onDown);
    canvas.addEventListener("pointermove", onMove);
    canvas.addEventListener("pointerup", onUp);
    canvas.addEventListener("pointerleave", onLeave);
    canvas.addEventListener("wheel", onWheel, { passive: false });

    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
      canvas.removeEventListener("pointerdown", onDown);
      canvas.removeEventListener("pointermove", onMove);
      canvas.removeEventListener("pointerup", onUp);
      canvas.removeEventListener("pointerleave", onLeave);
      canvas.removeEventListener("wheel", onWheel);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const resetView = () => {
    const s = stateRef.current;
    if (!s) return;
    s.yaw = 0.6;
    s.pitch = -0.35;
    s.zoom = 1;
    s.alpha = Math.max(s.alpha, 0.4);
  };

  return (
    <>
      <div className="graph-bar">
        <span className="hint" style={{ padding: 0 }}>
          {focusNode != null
            ? "drag to rotate · scroll to zoom · hover a node for its row · click to drill deeper"
            : "drag to rotate · scroll to zoom · hover a node for its row · click to focus"}
        </span>
        <span style={{ flex: 1 }} />
        {focusNode != null && (
          <button className="ghost sm" onClick={() => onFocus(null)} title="show the whole graph">
            {g.focusMissing ? `focus ${focusNode} (not here) ✕` : `▸ ${focusNode} ✕`}
          </button>
        )}
        <button
          className={"ghost sm" + (auto ? " on" : "")}
          title="toggle auto-rotate"
          onClick={() => setAuto((a) => !a)}
        >
          auto-rotate
        </button>
        <button className="ghost sm" title="reset rotation / zoom" onClick={resetView}>
          reset view
        </button>
      </div>
      <div className="chart-canvas" ref={canvasRef}>
        <canvas ref={canvasEl} style={{ display: "block", cursor: "grab", touchAction: "none" }} />
      </div>
      {legend && <GraphColorLegend legend={legend} />}
      {g.total > NODE_CAP && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          showing first {NODE_CAP} of {g.total} nodes — add a tighter <code>WHERE</code>/
          <code>LIMIT</code>.
          {!showLabels && " labels hidden (too many nodes); hover for detail."}
        </div>
      )}
    </>
  );

  // ---- rendering ----
  function draw(ctx: CanvasRenderingContext2D, s: RenderState) {
    // Advance the layout while it is still warm, then cool.
    if (s.alpha > 0.02 && s.n > 0) {
      stepForces(s.pos, s.edges, s.alpha * 0.5);
      s.alpha *= 0.985;
    }
    if (s.auto && !s.dragging) s.yaw += 0.0025;

    const { w, h, dpr } = s;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);

    const n = s.pos.length;
    if (n === 0) return;

    const cosY = Math.cos(s.yaw);
    const sinY = Math.sin(s.yaw);
    const cosP = Math.cos(s.pitch);
    const sinP = Math.sin(s.pitch);

    // Rotate every node; track the max in-plane extent to auto-fit the view.
    const rx = new Float64Array(n);
    const ry = new Float64Array(n);
    const rz = new Float64Array(n);
    let maxAbs = 1e-6;
    for (let i = 0; i < n; i++) {
      const p = s.pos[i];
      const x1 = p.x * cosY - p.z * sinY;
      const z1 = p.x * sinY + p.z * cosY;
      const y1 = p.y * cosP - z1 * sinP;
      const z2 = p.y * sinP + z1 * cosP;
      rx[i] = x1;
      ry[i] = y1;
      rz[i] = z2;
      maxAbs = Math.max(maxAbs, Math.abs(x1), Math.abs(y1), Math.abs(z2));
    }
    const focal = maxAbs * 2.6; // perspective camera distance
    const cx = w / 2;
    const cy = h / 2;
    const fit = (Math.min(w, h) * 0.42 * s.zoom) / maxAbs;

    // Project with perspective; nearer nodes (smaller depth) get a larger ps.
    const proj: Proj[] = new Array(n);
    for (let i = 0; i < n; i++) {
      const ps = focal / (focal + rz[i]);
      proj[i] = {
        i,
        ps,
        sx: cx + rx[i] * fit * ps,
        sy: cy + ry[i] * fit * ps,
        r: i === s.rootIndex ? 0 : Math.max(1.5, s.radii[i] * (0.55 + 0.7 * ps) * Math.sqrt(s.zoom)),
      };
    }
    s.proj = proj;

    // Edges first (behind nodes), dimmed by average depth.
    ctx.lineWidth = 1;
    for (const e of s.edges) {
      const a = proj[e.source];
      const b = proj[e.target];
      if (!a || !b) continue;
      const al = 0.1 + 0.28 * ((a.ps + b.ps) / 2);
      ctx.strokeStyle = `rgba(${EDGE_COLOR},${al.toFixed(3)})`;
      ctx.beginPath();
      ctx.moveTo(a.sx, a.sy);
      ctx.lineTo(b.sx, b.sy);
      ctx.stroke();
    }

    // Nodes back-to-front (painter's algorithm) so near nodes overlap far ones.
    const order = proj
      .filter((p) => p.i !== s.rootIndex)
      .sort((p, q) => rz[q.i] - rz[p.i]);
    ctx.textAlign = "center";
    ctx.textBaseline = "bottom";
    ctx.font = "10px ui-monospace, Menlo, monospace";
    for (const p of order) {
      ctx.globalAlpha = 0.4 + 0.6 * p.ps;
      ctx.beginPath();
      ctx.arc(p.sx, p.sy, p.r, 0, Math.PI * 2);
      ctx.fillStyle = s.colors[p.i];
      ctx.fill();
      ctx.lineWidth = 1;
      ctx.strokeStyle = "rgba(138,147,167,0.35)";
      ctx.stroke();
    }
    if (s.showLabels) {
      ctx.globalAlpha = 1;
      ctx.fillStyle = "#c8cdd8";
      for (const p of order) {
        if (p.ps < 0.85) continue; // label only the nearer nodes to cut clutter
        const label = s.labels[p.i];
        if (label) ctx.fillText(label, p.sx, p.sy - p.r - 1);
      }
    }
    ctx.globalAlpha = 1;

    // Hover: hit-test against *this* frame's projection (the layout and camera
    // move under a still pointer), then ring the node and draw its record.
    s.hoverIdx =
      s.dragging || s.hoverX === null || s.hoverY === null
        ? -1
        : hitTest(proj, s.hoverX, s.hoverY);
    const cursor = s.hoverIdx >= 0 ? "pointer" : "grab";
    if (ctx.canvas.style.cursor !== cursor) ctx.canvas.style.cursor = cursor;
    if (s.hoverIdx >= 0) {
      const p = proj[s.hoverIdx];
      ctx.beginPath();
      ctx.arc(p.sx, p.sy, p.r + 3, 0, Math.PI * 2);
      ctx.strokeStyle = "#c8cdd8";
      ctx.lineWidth = 1.5;
      ctx.stroke();
      drawTip(ctx, s, s.hoverIdx);
    }
  }

  // drawTip paints the hovered node's detail panel: its label (plus colour/size
  // measures) and, when row detail is on, the source row behind it. Rendered on
  // the canvas because this view has no DOM elements to hang a tooltip off.
  function drawTip(ctx: CanvasRenderingContext2D, s: RenderState, i: number) {
    if (s.tipIdx !== i) {
      const head = [s.labels[i] || s.keys[i]];
      if (s.colorName) head[0] += ` · ${s.colorName}`;
      if (s.sizes[i] !== null) head[0] += ` (${s.sizes[i]})`;
      const ri = s.nodeRows[i];
      const body =
        s.rowDetail && ri != null && s.rows[ri] ? rowLines(s.rows[ri], s.columns) : [];
      s.tipLines = body.length ? [...head, ...body] : head;
      s.tipIdx = i;
    }
    const lines = s.tipLines;
    if (!lines.length) return;

    const PAD = 7;
    const LH = 14;
    ctx.font = "11px ui-monospace, Menlo, monospace";
    ctx.textAlign = "left";
    ctx.textBaseline = "top";
    let wide = 0;
    for (const l of lines) wide = Math.max(wide, ctx.measureText(l).width);
    const bw = wide + PAD * 2;
    const bh = lines.length * LH + PAD * 2;
    // Anchor below-right of the pointer, flipping/clamping to stay on canvas.
    let bx = (s.hoverX ?? 0) + 14;
    let by = (s.hoverY ?? 0) + 14;
    if (bx + bw > s.w) bx = Math.max(2, (s.hoverX ?? 0) - 14 - bw);
    if (by + bh > s.h) by = Math.max(2, s.h - bh - 2);

    ctx.fillStyle = "rgba(23,26,33,0.94)";
    ctx.strokeStyle = "rgba(138,147,167,0.35)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    if (ctx.roundRect) ctx.roundRect(bx, by, bw, bh, 4);
    else ctx.rect(bx, by, bw, bh);
    ctx.fill();
    ctx.stroke();

    lines.forEach((l, k) => {
      ctx.fillStyle = k === 0 ? "#e6e9ef" : "#a9b1c2";
      ctx.fillText(l, bx + PAD, by + PAD + k * LH);
    });
  }
});
