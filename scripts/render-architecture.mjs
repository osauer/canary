#!/usr/bin/env node

// Deterministic, dependency-free renderer for the public architecture diagrams.
// Visual tokens mirror docs/shared.css so the diagrams read as part of the site.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), "..");
// Diagrams are published assets, so they are written into the site tree rather
const outDir = path.join(repoRoot, "docs/diagrams");
const canary = fs.readFileSync(path.join(repoRoot, "docs/social/canary-icon.png")).toString("base64");

const icons = {
  user: `<path d="M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0"/><path d="M6 21v-2a4 4 0 0 1 4 -4h4a4 4 0 0 1 4 4v2"/>`,
  cpu: `<path d="M5 6a1 1 0 0 1 1 -1h12a1 1 0 0 1 1 1v12a1 1 0 0 1 -1 1h-12a1 1 0 0 1 -1 -1l0 -12"/><path d="M8 10v-2h2m6 6v2h-2m-4 0h-2v-2m8 -4v-2h-2"/><path d="M3 10h2M3 14h2M10 3v2M14 3v2M21 10h-2M21 14h-2M14 21v-2M10 21v-2"/>`,
  terminal: `<path d="M8 9l3 3l-3 3M13 15h3"/><path d="M3 6a2 2 0 0 1 2 -2h14a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2h-14a2 2 0 0 1 -2 -2z"/>`,
  plugConnected: `<path d="M7 12l5 5l-1.5 1.5a3.536 3.536 0 1 1 -5 -5l1.5 -1.5M17 12l-5 -5l1.5 -1.5a3.536 3.536 0 1 1 5 5l-1.5 1.5M3 21l2.5 -2.5M18.5 5.5l2.5 -2.5M10 11l-2 2M13 14l-2 2"/>`,
  mobileCode: `<path d="M11.5 21h-3.5a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h8a2 2 0 0 1 2 2v8M20 21l2 -2l-2 -2M17 17l-2 2l2 2M11 4h2M12 17v.01"/>`,
  exchange: `<path d="M7 10h14l-4 -4M17 14h-14l4 4"/>`,
  serverCog: `<path d="M3 7a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3v-2M12 20h-6a3 3 0 0 1 -3 -3v-2a3 3 0 0 1 3 -3h10.5"/><path d="M16 18a2 2 0 1 0 4 0a2 2 0 1 0 -4 0M18 14.5v1.5M18 20v1.5M21.032 16.25l-1.299 .75M16.27 19l-1.3 .75M14.97 16.25l1.3 .75M19.733 19l1.3 .75M7 8v.01M7 16v.01"/>`,
  shieldCheck: `<path d="M11.46 20.846a12 12 0 0 1 -7.96 -14.846a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 -.09 7.06M15 19l2 2l4 -4"/>`,
  plug: `<path d="M9.785 6l8.215 8.215l-2.054 2.054a5.81 5.81 0 1 1 -8.215 -8.215zM4 20l3.5 -3.5M15 4l-3.5 3.5M20 9l-3.5 3.5"/>`,
  databaseImport: `<path d="M4 6c0 1.657 3.582 3 8 3s8 -1.343 8 -3s-3.582 -3 -8 -3s-8 1.343 -8 3M4 6v6c0 1.657 3.582 3 8 3c.856 0 1.68 -.05 2.454 -.144M20 12v-6M4 12v6c0 1.657 3.582 3 8 3c.171 0 .341 -.002 .51 -.006M19 22v-6M22 19l-3 -3l-3 3"/>`,
  database: `<path d="M4 6a8 3 0 1 0 16 0a8 3 0 1 0 -16 0M4 6v6a8 3 0 0 0 16 0v-6M4 12v6a8 3 0 0 0 16 0v-6"/>`,
  server: `<path d="M3 7a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3M3 15a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3zM7 8v.01M7 16v.01"/>`,
  worldDownload: `<path d="M21 12a9 9 0 1 0 -9 9M3.6 9h16.8M3.6 15h8.4M11.578 3a17 17 0 0 0 0 18M12.5 3c1.719 2.755 2.5 5.876 2.5 9M18 14v7m-3 -3l3 3l3 -3"/>`,
  cloud: `<path d="M6.657 18c-2.572 0 -4.657 -2.007 -4.657 -4.483c0 -2.475 2.085 -4.482 4.657 -4.482c.393 -1.762 1.794 -3.2 3.675 -3.773c1.88 -.572 3.956 -.193 5.444 1c1.488 1.19 2.162 3.007 1.77 4.769h.99c1.913 0 3.464 1.56 3.464 3.486c0 1.927 -1.551 3.487 -3.465 3.487h-11.878"/>`,
  bell: `<path d="M10 5a2 2 0 1 1 4 0a7 7 0 0 1 4 6v3a4 4 0 0 0 2 3h-16a4 4 0 0 0 2 -3v-3a7 7 0 0 1 4 -6M9 17v1a3 3 0 0 0 6 0v-1"/>`,
  calendarCode: `<path d="M11.5 21h-5.5a2 2 0 0 1 -2 -2v-12a2 2 0 0 1 2 -2h12a2 2 0 0 1 2 2v6M16 3v4M8 3v4M4 11h16M20 21l2 -2l-2 -2M17 17l-2 2l2 2"/>`,
  settings: `<path d="M10.325 4.317c.426 -1.756 2.924 -1.756 3.35 0a1.724 1.724 0 0 0 2.573 1.066c1.543 -.94 3.31 .826 2.37 2.37a1.724 1.724 0 0 0 1.065 2.572c1.756 .426 1.756 2.924 0 3.35a1.724 1.724 0 0 0 -1.066 2.573c.94 1.543 -.826 3.31 -2.37 2.37a1.724 1.724 0 0 0 -2.572 1.065c-.426 1.756 -2.924 1.756 -3.35 0a1.724 1.724 0 0 0 -2.573 -1.066c-1.543 .94 -3.31 -.826 -2.37 -2.37a1.724 1.724 0 0 0 -1.065 -2.572c-1.756 -.426 -1.756 -2.924 0 -3.35a1.724 1.724 0 0 0 1.066 -2.573c-.94 -1.543 .826 -3.31 2.37 -2.37c1 .608 2.296 .07 2.572 -1.065M9 12a3 3 0 1 0 6 0a3 3 0 0 0 -6 0"/>`,
  fileText: `<path d="M14 3v4a1 1 0 0 0 1 1h4M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2M9 9h1M9 13h6M9 17h6"/>`,
  refresh: `<path d="M20 11a8.1 8.1 0 0 0 -15.5 -2M4 5v4h4M4 13a8.1 8.1 0 0 0 15.5 2M20 19v-4h-4"/>`,
  browser: `<path d="M4 8h16M4 6a2 2 0 0 1 2 -2h12a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2h-12a2 2 0 0 1 -2 -2zM8 4v4"/>`,
  lock: `<path d="M5 13a2 2 0 0 1 2 -2h10a2 2 0 0 1 2 2v6a2 2 0 0 1 -2 2h-10a2 2 0 0 1 -2 -2zM11 16a1 1 0 1 0 2 0a1 1 0 0 0 -2 0M8 11v-4a4 4 0 1 1 8 0v4"/>`,
  route: `<path d="M3 19a2 2 0 1 0 4 0a2 2 0 0 0 -4 0M19 7a2 2 0 1 0 0 -4a2 2 0 0 0 0 4M11 19h5.5a3.5 3.5 0 0 0 0 -7h-8a3.5 3.5 0 0 1 0 -7h4.5"/>`,
};

// Tokens mirror docs/shared.css. Flow hues (green/blue/amber) are validated
const C = {
  paper: "#f7f5ef",
  panel: "#fffdf7",
  panelAlt: "#ece7db",
  ink: "#101827",
  muted: "#42526a",
  line: "#d8d2c4",
  terminal: "#0e1626",
  terminal2: "#1b2a45",
  terminalLine: "#26364c",
  textOnDark: "#c9d4e6",
  textOnDarkDim: "#8fa3c2",
  slate: "#42526a",
  green: "#0a8a72",
  greenDark: "#055f52",
  greenSoft: "#e3f0ec",
  greenLine: "#a8cdc2",
  blue: "#2f5fa5",
  blueSoft: "#e8eef7",
  amber: "#b45309",
  amberSoft: "#f9efdd",
  yellow: "#f5c542",
};

const esc = (value) => String(value)
  .replaceAll("&", "&amp;")
  .replaceAll("<", "&lt;")
  .replaceAll(">", "&gt;")
  .replaceAll('"', "&quot;");

function defs(extraStyles = "") {
  const symbols = Object.entries(icons).map(([name, body]) =>
    `<symbol id="icon-${name}" viewBox="0 0 24 24">${body}</symbol>`).join("");
  const marker = (name, color) => `<marker id="arrow-${name}" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="${color}"/></marker>`;
  return `<defs>${symbols}${marker("slate", C.slate)}${marker("green", C.green)}${marker("blue", C.blue)}${marker("amber", C.amber)}
    <style>
      text { font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; fill: ${C.ink}; }
      .kicker { font-size: 13px; font-weight: 700; fill: ${C.muted}; letter-spacing: .4px; }
      .title { font-size: 28px; font-weight: 700; letter-spacing: -.4px; }
      .subtitle { font-size: 14px; fill: ${C.muted}; }
      .layer { font-size: 12px; font-weight: 700; letter-spacing: 1.1px; fill: ${C.muted}; }
      .boundary { font-size: 12px; font-weight: 700; letter-spacing: 1.1px; fill: ${C.slate}; }
      .node-title { font-size: 15px; font-weight: 650; }
      .node-sub { font-size: 12.5px; fill: ${C.muted}; }
      .on-dark { fill: ${C.textOnDark}; }
      .on-dark-dim { fill: ${C.textOnDarkDim}; }
      .module-title { font-size: 13.5px; font-weight: 650; fill: #ffffff; }
      .module-sub { font-size: 12px; fill: ${C.textOnDark}; }
      .mono { font: 11.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .mono-small { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .flow-label { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.amber}; }
      .legend { font-size: 11.5px; fill: ${C.muted}; }
      .strip-node { font-size: 12px; font-weight: 650; fill: ${C.ink}; }
      .matrix-head { font-size: 13px; font-weight: 650; fill: ${C.ink}; }
      .matrix-sub { font-size: 11.5px; fill: ${C.muted}; }
      .matrix-owner { font-size: 15px; font-weight: 650; fill: ${C.ink}; }
      .tile-title { font-size: 13.5px; font-weight: 650; fill: ${C.ink}; }
      .tile-sub { font-size: 11.8px; fill: ${C.muted}; }
      .format { font: 10.8px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .footnote { font-size: 10.5px; fill: ${C.muted}; opacity: .85; }
${extraStyles}    </style>
  </defs>`;
}

function icon(name, x, y, size, color = "#ffffff", strokeWidth = 1.8) {
  return `<svg x="${x}" y="${y}" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="${strokeWidth}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#icon-${name}"/></svg>`;
}

function iconTile(name, x, y, color, size = 44, iconColor = "#ffffff") {
  const pad = Math.round(size * 0.23);
  return `<rect x="${x}" y="${y}" width="${size}" height="${size}" rx="${Math.round(size / 4)}" fill="${color}"/>${icon(name, x + pad, y + pad, size - 2 * pad, iconColor)}`;
}

function textLines(x, y, lines, className, gap = 16) {
  if (!lines.length) return "";
  return `<text x="${x}" y="${y}" class="${className}">${lines.map((line, index) => `<tspan x="${x}" dy="${index ? gap : 0}">${esc(line)}</tspan>`).join("")}</text>`;
}

// Borderless component: icon tile, title, plain sub lines, optional mono line,
function component({ x, y, iconName, color, iconColor = "#ffffff", title, subtitle = [], mono = "", width = 190 }) {
  const labelX = x + 56;
  const monoY = y + 36 + subtitle.length * 15;
  return `<g role="group" aria-label="${esc([title, ...subtitle, mono].filter(Boolean).join(". "))}">
    ${iconTile(iconName, x, y, color, 44, iconColor)}
    <text x="${labelX}" y="${y + 17}" class="node-title">${esc(title)}</text>
    ${textLines(labelX, y + 36, subtitle, "node-sub", 15)}
    ${mono ? `<text x="${labelX}" y="${monoY}" class="mono-small">${esc(mono)}</text>` : ""}
    <rect x="${x}" y="${y + 68}" width="${width}" height="1" fill="${C.line}"/>
  </g>`;
}

function line(d, colorName = "slate", { dashed = false, dotted = false, both = false, width = 1.8 } = {}) {
  const color = C[colorName];
  const dash = dotted ? ' stroke-dasharray="2 5"' : dashed ? ' stroke-dasharray="7 6"' : "";
  return `<path d="${d}" fill="none" stroke="${color}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round"${dash} marker-end="url(#arrow-${colorName})"${both ? ` marker-start="url(#arrow-${colorName})"` : ""}/>`;
}

// Width of a mono chip label at 11.5px (~6.91px/char) plus pill padding.
const chipW = (label) => Math.round(label.length * 6.91 + 22);

function chip(x, y, width, label, fill = C.panelAlt, textClass = "mono") {
  return `<g><rect x="${x}" y="${y}" width="${width}" height="24" rx="12" fill="${fill}"/><text x="${x + width / 2}" y="${y + 16}" text-anchor="middle" class="${textClass}">${esc(label)}</text></g>`;
}

// Mono chip centered on cx with measured width.
function chipAt(cx, y, label, fill = C.panelAlt) {
  const width = chipW(label);
  return chip(Math.round(cx - width / 2), y, width, label, fill);
}

function legendItem(x, y, colorName, label, { dashed = false, dotted = false } = {}) {
  const dash = dotted ? ' stroke-dasharray="2 5"' : dashed ? ' stroke-dasharray="6 5"' : "";
  return `<line x1="${x}" y1="${y}" x2="${x + 28}" y2="${y}" stroke="${C[colorName]}" stroke-width="3" stroke-linecap="round"${dash}/><text x="${x + 36}" y="${y + 4}" class="legend">${esc(label)}</text>`;
}

function header(title, subtitle) {
  return `
  <image href="data:image/png;base64,${canary}" x="36" y="26" width="46" height="46" preserveAspectRatio="xMidYMid slice"/>
  <text x="96" y="42" class="kicker">Canary</text>
  <text x="96" y="70" class="title">${esc(title)}</text>
  <text x="96" y="92" class="subtitle">${esc(subtitle)}</text>`;
}

function junction(x, y, color = C.slate) {
  return `<circle cx="${x}" cy="${y}" r="3" fill="${color}"/>`;
}

function svgFrame({ width, height, title, description, body, extraStyles = "" }) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-labelledby="diagram-title diagram-desc">
  <title id="diagram-title">${esc(title)}</title>
  <desc id="diagram-desc">${esc(description)}</desc>
  <metadata>Generic component icons derived from Tabler Icons 3.45.0, MIT License. Canary mark copyright the Canary project.</metadata>
  ${defs(extraStyles)}
  <rect width="${width}" height="${height}" fill="${C.paper}"/>
  ${body}
</svg>\n`;
}

const referenceDiagramStyles = `      .card-title { font-size: 14px; font-weight: 700; fill: ${C.ink}; }
      .card-sub { font-size: 11.5px; fill: ${C.muted}; }
      .er-title { font-size: 13px; font-weight: 700; fill: ${C.ink}; }
      .er-key { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-weight: 700; fill: ${C.greenDark}; }
      .er-field { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .er-cardinality { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-weight: 700; fill: ${C.slate}; }
      .er-semantic { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.blue}; }
      .step-no { font-size: 12px; font-weight: 800; fill: #ffffff; }
      .step-title { font-size: 12.5px; font-weight: 700; fill: ${C.ink}; }
      .step-sub { font-size: 10.8px; fill: ${C.muted}; }
`;

const policyPrimerStyles = `${referenceDiagramStyles}      .primer-title { font-size: 13px; font-weight: 700; fill: ${C.ink}; }
      .primer-sub { font-size: 11.2px; fill: ${C.muted}; }
      .primer-note { font-size: 11px; font-weight: 650; fill: ${C.greenDark}; }
`;

const policyReadableStyles = `${referenceDiagramStyles}      .card-title { font-size: 14.5px; font-weight: 700; }
      .card-sub { font-size: 13px; }
      .card-note { font-size: 11.5px; fill: ${C.muted}; }
      .node-title { font-size: 15px; font-weight: 700; }
      .node-sub { font-size: 13px; }
      .mono-small { font-size: 11.5px; }
      .layer { font-size: 12.5px; }
      .policy-callout { font-size: 14px; font-weight: 750; fill: ${C.ink}; }
      .policy-table-head { font-size: 12.5px; font-weight: 750; fill: ${C.muted}; letter-spacing: .35px; }
      .policy-table-text { font-size: 12.75px; fill: ${C.ink}; }
      .policy-table-sub { font-size: 12.5px; fill: ${C.muted}; }
`;

const storageOverviewStyles = `${policyPrimerStyles}      .card-title { font-size: 16px; }
      .card-sub { font-size: 14px; }
      .mono-small { font-size: 12.5px; }
      .module-title { font-size: 15px; }
      .module-sub { font-size: 13.5px; }
      .layer { font-size: 13px; }
`;

const storageERStyles = `${referenceDiagramStyles}      .er-title { font-size: 13.5px; }
      .er-key { font-size: 12.25px; }
      .er-field { font-size: 12.25px; }
      .er-cardinality { font-size: 12.25px; }
      .er-semantic { font-size: 12px; }
      .er-note { font-size: 12.5px; fill: ${C.muted}; }
      .layer { font-size: 12.5px; }
`;

function stripNode(x, y, width, label, iconName = "") {
  const textX = iconName ? x + width / 2 + 11 : x + width / 2;
  return `<g>
    <rect x="${x}" y="${y}" width="${width}" height="28" rx="14" fill="${C.panel}" stroke="${C.amber}" stroke-width="1.2"/>
    ${iconName ? icon(iconName, x + 12, y + 6, 16, C.amber, 2) : ""}
    <text x="${textX}" y="${y + 18.5}" text-anchor="middle" class="strip-node">${esc(label)}</text>
  </g>`;
}

function systemArchitecture() {
  const rows = [210, 352, 494];        // consumer/adapter tile tops
  const arrowY = rows.map((y) => y + 22);
  const intRows = [212, 324, 436, 548]; // integration tile tops
  const intArrowY = intRows.map((y) => y + 22);
  const busX = 884;

  const body = `
  ${header("Runtime Architecture", "One daemon owns the broker session; typed adapters serve humans, AI hosts, and the app.")}

  ${legendItem(1157, 40, "slate", "Local typed flow")}
  ${legendItem(1304, 40, "green", "Broker path")}
  ${legendItem(1146, 64, "blue", "Observed data", { dashed: true })}
  ${legendItem(1281, 64, "amber", "Optional remote", { dotted: true })}

  <rect x="224" y="120" width="916" height="548" rx="16" fill="${C.panel}" stroke="${C.muted}" stroke-width="1.2"/>
  <path d="M224 156h916v-20a16 16 0 0 0 -16 -16h-884a16 16 0 0 0 -16 16z" fill="${C.panelAlt}"/>
  <text x="244" y="143" class="boundary">LOCAL CANARY RUNTIME</text>
  <text x="1120" y="143" text-anchor="end" class="mono-small">one binary · several processes · one shared daemon</text>

  <text x="36" y="184" class="layer">1 · CONSUMERS</text>
  <text x="246" y="184" class="layer">2 · SURFACE ADAPTERS</text>
  <text x="452" y="184" class="layer">3 · CONTRACT</text>
  <text x="584" y="184" class="layer">4 · AUTHORITY &amp; DOMAIN</text>
  <text x="898" y="184" class="layer">5 · INTEGRATIONS</text>
  <text x="1164" y="184" class="layer">6 · PROVIDERS / DATA</text>

  ${component({ x: 36, y: rows[0], iconName: "user", color: C.slate, title: "Human Operator", subtitle: ["shell · local browser"], width: 172 })}
  ${component({ x: 36, y: rows[1], iconName: "cpu", color: C.slate, title: "AI / MCP Host", subtitle: ["Claude · Codex", "other MCP clients"], width: 172 })}
  ${component({ x: 36, y: rows[2], iconName: "mobileCode", color: C.yellow, iconColor: C.ink, title: "Canary PWA", subtitle: ["browser", "iOS Home Screen"], width: 172 })}

  ${component({ x: 246, y: rows[0], iconName: "terminal", color: C.slate, title: "CLI", subtitle: ["argv · stdout"], mono: "internal/cli", width: 180 })}
  ${component({ x: 246, y: rows[1], iconName: "plugConnected", color: C.slate, title: "MCP Adapter", subtitle: ["JSON-RPC 2.0 · stdio"], mono: "internal/mcp", width: 180 })}
  ${component({ x: 246, y: rows[2], iconName: "mobileCode", color: C.yellow, iconColor: C.ink, title: "Canary App Host", subtitle: ["HTTP JSON · SSE"], mono: "internal/app", width: 180 })}

  ${line(`M214 ${arrowY[0]}H240`, "slate")}
  ${line(`M214 ${arrowY[1]}H240`, "slate")}
  ${line(`M214 ${arrowY[2]}H240`, "slate", { both: true })}

  <rect x="452" y="200" width="110" height="450" rx="14" fill="${C.greenSoft}" stroke="${C.greenLine}"/>
  ${icon("exchange", 492, 222, 30, C.greenDark, 1.8)}
  <text x="507" y="286" text-anchor="middle" class="node-title"><tspan x="507">Typed</tspan><tspan x="507" dy="17">RPC</tspan></text>
  ${chipAt(507, 320, "internal/rpc", C.panel)}
  <text x="507" y="376" text-anchor="middle" class="node-sub"><tspan x="507">NDJSON</tspan><tspan x="507" dy="16">frames</tspan><tspan x="507" dy="16">Unix socket</tspan></text>
  <text x="507" y="566" text-anchor="middle" class="node-sub"><tspan x="507">one shared</tspan><tspan x="507" dy="16">contract</tspan><tspan x="507" dy="16">boundary</tspan></text>

  ${line(`M432 ${arrowY[0]}H446`, "slate")}
  ${line(`M432 ${arrowY[1]}H446`, "slate")}
  ${line(`M432 ${arrowY[2]}H446`, "slate")}
  ${line("M562 420H578", "slate")}

  <rect x="584" y="200" width="290" height="450" rx="16" fill="${C.terminal}"/>
  ${iconTile("serverCog", 608, 224, C.green, 46)}
  <text x="668" y="246" style="fill:#ffffff;font-size:18px;font-weight:700">canary daemon</text>
  <text x="668" y="267" class="node-sub on-dark">broker + runtime authority</text>

  <rect x="608" y="300" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("settings", 620, 315, 26, "#9fb3d9")}
  <text x="658" y="325" class="module-title">Runtime Orchestration</text>
  <text x="658" y="345" class="module-sub">schedulers · policy execution</text>

  <rect x="608" y="376" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("shieldCheck", 620, 391, 26, "#6fd3c2")}
  <text x="658" y="401" class="module-title">Pure Risk Semantics</text>
  <text x="658" y="421" class="module-sub">internal/risk · no I/O</text>

  <rect x="608" y="452" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("fileText", 620, 467, 26, C.yellow)}
  <text x="658" y="477" class="module-title">State Lifecycle</text>
  <text x="658" y="497" class="module-sub">SQLite events · recon · proposals</text>

  <rect x="608" y="560" width="242" height="1" fill="${C.terminalLine}"/>
  <text x="608" y="588" class="mono-small" style="fill:${C.textOnDarkDim}">auto-spawned by clients on demand</text>
  <text x="608" y="606" class="mono-small" style="fill:${C.textOnDarkDim}">default idle timeout 15 min</text>

  ${component({ x: 898, y: intRows[0], iconName: "plug", color: C.green, title: "Broker Connectors", subtitle: ["primary + breadth clients"], mono: "pkg/ibkr", width: 226 })}
  ${component({ x: 898, y: intRows[1], iconName: "databaseImport", color: C.blue, title: "Observed-Source Clients", subtitle: ["Flex · market events · rates"], width: 226 })}
  ${component({ x: 898, y: intRows[2], iconName: "database", color: C.greenDark, title: "Persistence", subtitle: ["daemon.db · evidence · app/data"], width: 226 })}
  ${component({ x: 898, y: intRows[3], iconName: "calendarCode", color: C.slate, title: "Embedded Calendars", subtitle: ["official sessions 2026-2028", "compiled into the binary"], width: 226 })}

  <path d="M874 420H${busX}M${busX} ${intArrowY[0]}V${intArrowY[3]}" fill="none" stroke="${C.slate}" stroke-width="1.5" stroke-linecap="round"/>
  ${junction(busX, 420)}
  ${line(`M${busX} ${intArrowY[0]}H892`, "green")}
  ${line(`M${busX} ${intArrowY[1]}H892`, "blue", { dashed: true })}
  ${line(`M${busX} ${intArrowY[2]}H892`, "slate")}
  ${line(`M898 ${intArrowY[3]}H${busX + 6}`, "slate", { dotted: true })}

  ${component({ x: 1164, y: 212, iconName: "server", color: C.green, title: "TWS / IB Gateway", subtitle: ["selected endpoint + account"], width: 240 })}
  ${chipAt(1284, 292, "TWS wire · TCP/TLS · 2 client IDs", C.greenSoft)}
  ${component({ x: 1164, y: 362, iconName: "worldDownload", color: C.blue, title: "IBKR Data Services", subtitle: ["Flex statements · borrow data"], width: 240 })}
  ${component({ x: 1164, y: 512, iconName: "worldDownload", color: C.blue, title: "Public Market Sources", subtitle: ["Nasdaq · FRED · CBOE · Treasury", "Fed · Wikipedia S&P 500 list"], width: 240 })}
  ${chipAt(1284, 604, "HTTPS · FTP · JSON/CSV/XML/RSS", C.blueSoft)}

  ${line(`M1124 ${intArrowY[0]}H1154`, "green", { both: true })}
  ${line(`M1124 ${intArrowY[1]}H1140V384H1154`, "blue", { dashed: true })}
  ${line(`M1140 ${intArrowY[1]}V534H1154`, "blue", { dashed: true })}
  ${junction(1140, intArrowY[1], C.blue)}

  <rect x="36" y="700" width="700" height="112" rx="14" fill="${C.amberSoft}" stroke="${C.amber}" stroke-width="1.2" stroke-dasharray="8 6"/>
  <text x="56" y="726" class="layer" style="fill:${C.amber}">OPTIONAL REMOTE DELIVERY</text>
  <text x="716" y="726" text-anchor="end" class="legend">pairing, auth, and allowlists stay on the local app</text>

  ${stripNode(56, 742, 104, "Canary PWA")}
  ${line("M168 756H222", "amber", { dotted: true, both: true, width: 1.6 })}
  <text x="195" y="748" text-anchor="middle" class="flow-label">HTTPS</text>
  ${stripNode(230, 742, 240, "Cloudflare Relay · Worker + DO", "cloud")}
  ${line("M478 756H568", "amber", { dotted: true, both: true, width: 1.6 })}
  <text x="523" y="748" text-anchor="middle" class="flow-label">outbound WSS</text>
  ${stripNode(576, 742, 84, "canary app")}

  ${stripNode(56, 778, 84, "canary app")}
  ${line("M148 792H244", "amber", { dotted: true, width: 1.6 })}
  <text x="196" y="788" text-anchor="middle" class="flow-label">VAPID Web Push</text>
  ${stripNode(252, 778, 190, "Browser Push Service", "bell")}
  ${line("M450 792H568", "amber", { dotted: true, width: 1.6 })}
  <text x="509" y="788" text-anchor="middle" class="flow-label">redacted payloads</text>
  ${stripNode(576, 778, 104, "Canary PWA")}

  <text x="1404" y="806" text-anchor="end" class="footnote">deterministic SVG · scripts/render-architecture.mjs · icons: Tabler 3.45 (MIT)</text>
  `;

  return svgFrame({
    width: 1440,
    height: 848,
    title: "Runtime Architecture (Canary)",
    description: "Six architecture layers show consumers, surface adapters, the typed RPC contract, daemon authority, integration clients, and external providers. Optional remote delivery is isolated from broker and observed-data paths.",
    body,
  });
}

function matrixTile({ x, y, width, height, iconName, color, accent, title, lines = [], format = "", sensitive = false, optional = false }) {
  return `<g role="group" aria-label="${esc([title, ...lines, format].filter(Boolean).join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="12" fill="${C.panel}" stroke="${optional ? C.amber : C.line}"${optional ? ' stroke-dasharray="7 5"' : ""}/>
    <rect x="${x + 12}" y="${y}" width="${width - 24}" height="4" rx="2" fill="${accent}"/>
    ${iconTile(iconName, x + 14, y + 16, color, 36)}
    <text x="${x + 64}" y="${y + 33}" class="tile-title">${esc(title)}</text>
    ${sensitive ? icon("lock", x + width - 30, y + 16, 16, C.amber, 2) : ""}
    ${textLines(x + 64, y + 54, lines, "tile-sub", 15)}
    ${format ? `<text x="${x + 14}" y="${y + height - 13}" class="format">${esc(format)}</text>` : ""}
  </g>`;
}

function emptyCell(x, y, width, height) {
  return `<text x="${x + width / 2}" y="${y + height / 2 + 4}" text-anchor="middle" class="subtitle">—</text>`;
}

function persistenceArchitecture() {
  const colW = 256;
  const xs = [182, 452, 722, 992];
  const rowTops = [178, 330, 584, 748];   // operator, daemon (tall: index tile), canary, relay
  const tileY = (row) => rowTops[row] + 18;

  const ownerCell = (row, iconName, color, name, sub, iconColor = "#ffffff") => `
    ${iconTile(iconName, 42, rowTops[row] + 24, color, 42, iconColor)}
    <text x="42" y="${rowTops[row] + 92}" class="matrix-owner">${esc(name)}</text>
    <text x="42" y="${rowTops[row] + 110}" class="matrix-sub">${esc(sub)}</text>`;

  const colHead = (index, head, sub) => `
    <text x="${xs[index] + colW / 2}" y="150" text-anchor="middle" class="matrix-head">${esc(head)}</text>
    <text x="${xs[index] + colW / 2}" y="167" text-anchor="middle" class="matrix-sub">${esc(sub)}</text>`;

  const body = `
  ${header("State Ownership and Lifecycle", "Who owns which state, where it lives, and what survives a restart.")}

  ${legendItem(890, 40, "green", "Durable authority / evidence")}
  ${legendItem(1135, 40, "blue", "Refreshable")}
  ${legendItem(820, 64, "slate", "Runtime / recovery-only")}
  ${legendItem(1035, 64, "amber", "Optional hosted", { dashed: true })}
  ${icon("lock", 1175, 55, 15, C.amber, 2)}
  <text x="1196" y="68" class="legend">Sensitive</text>

  <rect x="24" y="124" width="1232" height="766" rx="16" fill="${C.panel}" stroke="${C.line}"/>
  <path d="M24 178h1232v-38a16 16 0 0 0 -16 -16h-1200a16 16 0 0 0 -16 16z" fill="${C.panelAlt}"/>
  <text x="42" y="156" class="boundary">OWNER</text>
  ${colHead(0, "Authored / External", "human or external authority")}
  ${colHead(1, "Durable Product Authority", "survives process restart")}
  ${colHead(2, "Refreshable Runtime", "in-memory or source-refreshed")}
  ${colHead(3, "Runtime & Recovery", "IPC, logs, recovery-only")}

  <line x1="174" y1="124" x2="174" y2="890" stroke="${C.line}"/>
  <line x1="24" y1="330" x2="1256" y2="330" stroke="${C.line}"/>
  <line x1="24" y1="584" x2="541" y2="584" stroke="${C.line}"/>
  <line x1="889" y1="584" x2="1256" y2="584" stroke="${C.line}"/>
  <line x1="24" y1="748" x2="1256" y2="748" stroke="${C.line}"/>

  ${ownerCell(0, "user", C.slate, "Operator", "writes and approves")}
  ${matrixTile({ x: xs[0], y: tileY(0), width: colW, height: 118, iconName: "settings", color: C.greenDark, accent: C.greenDark, title: "Config & Policies", lines: ["gateway, account, client pins", "risk-policy.toml · flex-token"], format: "$XDG_CONFIG_HOME/ibkr · TOML", sensitive: true })}
  ${matrixTile({ x: xs[1], y: tileY(0), width: colW, height: 118, iconName: "fileText", color: C.green, accent: C.green, title: "No User-Data Authority", lines: ["retired exports sealed", "no live file store"], format: "none" })}
  ${emptyCell(xs[2], tileY(0), colW, 118)}
  ${emptyCell(xs[3], tileY(0), colW, 118)}

  ${ownerCell(1, "serverCog", C.terminal, "Daemon", "runtime authority")}
  ${matrixTile({ x: xs[0], y: tileY(1), width: colW, height: 118, iconName: "fileText", color: C.greenDark, accent: C.greenDark, title: "Evidence & Signer Key", lines: ["Flex XML · broker truth", "private preview signer"], format: "state root · XML/private key", sensitive: true })}
  ${matrixTile({ x: xs[1], y: tileY(1), width: colW, height: 218, iconName: "database", color: C.green, accent: C.green, title: "daemon.db Authority", lines: ["settings · safety state", "orders · events · tokens", "market · contract · membership"], format: "SQLite WAL · sole live authority", sensitive: true })}
  ${matrixTile({ x: xs[2], y: tileY(1), width: colW, height: 218, iconName: "refresh", color: C.blue, accent: C.blue, title: "Refreshable Runtime", lines: ["quotes · source fetches", "in-memory derived views", "no JSON cache authority"], format: "memory · refetched/rebuilt" })}
  ${matrixTile({ x: xs[3], y: tileY(1), width: colW, height: 104, iconName: "route", color: C.slate, accent: C.slate, title: "Socket, Locks & Logs", lines: ["IPC and process locks", "rotated text logs"], format: "runtime/state directories" })}
  ${matrixTile({ x: xs[3], y: tileY(1) + 114, width: colW, height: 104, iconName: "shieldCheck", color: C.slate, accent: C.slate, title: "Recovery & Rollback", lines: [".head · verified backups", "hashed sealed legacy"], format: "fail-closed · never live fallback", sensitive: true })}

  <rect x="551" y="572" width="328" height="24" rx="12" fill="${C.panel}" stroke="${C.line}"/>
  ${icon("exchange", 566, 577, 14, C.slate, 2)}
  <text x="588" y="588" class="mono">typed RPC only · no direct DB/file access</text>

  ${ownerCell(2, "mobileCode", C.yellow, "Canary / Device", "separate app authority", C.ink)}
  ${emptyCell(xs[0], tileY(2), colW, 118)}
  ${matrixTile({ x: xs[1], y: tileY(2), width: colW, height: 118, iconName: "database", color: C.green, accent: C.green, title: "App State & Grants", lines: ["device grants · push · alerts", "VAPID · relay credentials"], format: "$XDG_STATE_HOME/ibkr/app · JSON", sensitive: true })}
  ${matrixTile({ x: xs[2], y: tileY(2), width: colW, height: 118, iconName: "refresh", color: C.blue, accent: C.blue, title: "Live App Snapshot", lines: ["periodic polls + quote streams", "memory-only read model"], format: "ephemeral" })}
  ${matrixTile({ x: xs[3], y: tileY(2), width: colW, height: 118, iconName: "browser", color: C.slate, accent: C.slate, title: "Browser / Device State", lines: ["cookies · IndexedDB · P-256 key", "continuity + local recovery"], format: "browser storage", sensitive: true })}

  ${ownerCell(3, "cloud", C.amber, "Hosted Relay", "transport only")}
  ${emptyCell(xs[0], tileY(3), colW, 106)}
  ${emptyCell(xs[1], tileY(3), colW, 106)}
  ${emptyCell(xs[2], tileY(3), colW, 106)}
  ${matrixTile({ x: xs[3], y: tileY(3), width: colW, height: 106, iconName: "cloud", color: C.amber, accent: C.amber, title: "Hosted Relay Route", lines: ["connector token + expiry", "no grants · no broker state"], format: "Cloudflare Durable Object", sensitive: true, optional: true })}

  <text x="1244" y="916" text-anchor="end" class="footnote">deterministic SVG · scripts/render-architecture.mjs · icons: Tabler 3.45 (MIT)</text>
  `;

  return svgFrame({
    width: 1280,
    height: 930,
    title: "State Ownership and Lifecycle (Canary)",
    description: "A matrix maps operator, daemon, Canary or device, and hosted relay ownership across authored or external authority, durable product authority, refreshable runtime views, and runtime or recovery-only state.",
    body,
  });
}

function policyCard({ x, y, width, height, iconName, color, title, lines, format, formatClass = "mono-small", dark = false }) {
  const fill = dark ? C.terminal : C.panel;
  const titleFill = dark ? "#ffffff" : C.ink;
  const subClass = dark ? "node-sub on-dark" : "card-sub";
  const formatFill = dark ? C.textOnDarkDim : C.muted;
  return `<g role="group" aria-label="${esc([title, ...lines, format].filter(Boolean).join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="14" fill="${fill}" stroke="${color}" stroke-width="1.2"/>
    ${iconTile(iconName, x + 16, y + 16, color, 38)}
    <text x="${x + 66}" y="${y + 35}" class="card-title" style="fill:${titleFill}">${esc(title)}</text>
    ${textLines(x + 18, y + 72, lines, subClass, 16)}
    ${format ? `<text x="${x + 18}" y="${y + height - 15}" class="${formatClass}" style="fill:${formatFill}">${esc(format)}</text>` : ""}
  </g>`;
}

function policyPrimer() {
  const body = `
  ${header("How Policy Informs a Trading Decision", "You set the boundary. Canary checks current evidence. You decide what happens next.")}

  <text x="36" y="140" class="layer">Boundary Set in Advance</text>
  <text x="290" y="204" class="layer">Canary Evaluation</text>
  <text x="526" y="204" class="layer">Current Effect</text>
  <text x="750" y="164" class="layer">Human Decision</text>

  ${policyCard({ x: 36, y: 164, width: 210, height: 138, iconName: "fileText", color: C.greenDark, title: "Risk Boundary", lines: ["capital at risk", "evidence required"], format: "Set by the trader", formatClass: "card-note" })}
  ${policyCard({ x: 36, y: 336, width: 210, height: 138, iconName: "worldDownload", color: C.blue, title: "Current Evidence", lines: ["account · market", "broker statement"], format: "Freshness included", formatClass: "card-note" })}
  ${policyCard({ x: 290, y: 228, width: 196, height: 196, iconName: "serverCog", color: C.green, title: "Policy Check", lines: ["policy + current evidence", "missing stays unknown", "one consistent result"], format: "No trading discretion", formatClass: "card-note", dark: true })}
  ${policyCard({ x: 526, y: 228, width: 196, height: 196, iconName: "bell", color: C.slate, title: "Advice Today", lines: ["status + explanation", "warning or proposal", "cannot submit"], format: "Advisory / shadow", formatClass: "card-note" })}

  <rect x="750" y="188" width="174" height="260" rx="16" fill="${C.amberSoft}" stroke="${C.amber}" stroke-width="1.5"/>
  ${iconTile("user", 766, 206, C.amber, 40)}
  <text x="814" y="216" class="node-title"><tspan x="814">Human</tspan><tspan x="814" dy="19">Decision</tspan></text>
  <text x="766" y="270" class="card-sub"><tspan x="766">wait · investigate</tspan><tspan x="766" dy="20">reduce risk · act</tspan></text>
  <rect x="766" y="330" width="142" height="94" rx="11" fill="${C.panel}" stroke="${C.amber}"/>
  ${icon("lock", 829, 340, 16, C.amber, 2)}
  <text x="837" y="374" text-anchor="middle" class="policy-callout"><tspan x="837">Only a human</tspan><tspan x="837" dy="17">authorizes</tspan><tspan x="837" dy="17">an order.</tspan></text>

  ${line("M246 233H274V276H290", "green")}
  ${line("M246 405H274V376H290", "blue", { dashed: true })}
  ${line("M486 326H526", "slate")}
  ${line("M722 326H750", "slate")}

  <rect x="290" y="500" width="260" height="118" rx="14" fill="${C.panel}" stroke="${C.slate}"/>
  ${iconTile("database", 306, 516, C.slate, 34)}
  <text x="354" y="536" class="node-title">Local Decision Record</text>
  <text x="306" y="574" class="card-sub"><tspan x="306">what Canary saw and concluded</tspan><tspan x="306" dy="19">not execution evidence</tspan></text>

  <rect x="580" y="500" width="344" height="118" rx="14" fill="${C.panel}" stroke="${C.blue}"/>
  ${iconTile("worldDownload", 596, 516, C.blue, 34)}
  <text x="644" y="536" class="node-title">Broker Evidence</text>
  <text x="596" y="574" class="card-sub"><tspan x="596">confirmation + statement</tspan><tspan x="596" dy="19">what actually executed</tspan></text>

  ${line("M624 424V474H420V500", "slate")}
  <path d="M837 448V486H752V500" fill="none" stroke="${C.amber}" stroke-width="1.6" stroke-dasharray="2 5" stroke-linecap="round" stroke-linejoin="round" marker-end="url(#arrow-amber)"/>
  <text x="846" y="478" class="card-note" style="fill:${C.amber}">If sent</text>

  <path d="M752 618V656H20V233H36" fill="none" stroke="${C.green}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" marker-end="url(#arrow-green)"/>
  <text x="446" y="650" text-anchor="middle" class="card-sub">Review broker outcomes before publishing a new policy version.</text>

  <text x="924" y="686" text-anchor="end" class="footnote">One trader may hold every role. Canary does not assign named roles.</text>
  `;

  return svgFrame({
    width: 960,
    height: 706,
    title: "How Policy Informs a Trading Decision (Canary)",
    description: "A trader sets the risk boundary, Canary checks it against current evidence, and the trader decides what happens next. Policy output is advisory or shadow, local decision records are not proof of execution, and only a human authorizes an order.",
    body,
    extraStyles: policyReadableStyles,
  });
}

function policyArchitecture() {
  const controlRow = (y, fill, control, provenance, effect, change) => `<g role="group" aria-label="${esc(`${control}. ${provenance}. ${effect}. ${change}`)}">
    <rect x="36" y="${y}" width="888" height="54" fill="${fill}" stroke="${C.line}"/>
    <text x="52" y="${y + 33}" class="policy-table-text" font-weight="700">${esc(control)}</text>
    <text x="298" y="${y + 33}" class="policy-table-text">${esc(provenance)}</text>
    <text x="502" y="${y + 33}" class="policy-table-text">${esc(effect)}</text>
    <text x="714" y="${y + 33}" class="policy-table-text">${esc(change)}</text>
  </g>`;
  const flowBox = (x, y, width, height, title, lines, accent, fill = C.panel, dark = false) => `<g role="group" aria-label="${esc([title, ...lines].join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="12" fill="${fill}" stroke="${accent}" stroke-width="1.3"/>
    <text x="${x + width / 2}" y="${y + 30}" text-anchor="middle" class="node-title" style="fill:${dark ? "#ffffff" : C.ink}">${esc(title)}</text>
    <text x="${x + width / 2}" y="${y + 58}" text-anchor="middle" class="${dark ? "node-sub on-dark" : "card-sub"}">${lines.map((line, index) => `<tspan x="${x + width / 2}" dy="${index ? 19 : 0}">${esc(line)}</tspan>`).join("")}</text>
  </g>`;

  const body = `
  ${header("Who Controls What", "Policies, settings, software, and broker gates each have a different job.")}

  <text x="36" y="138" class="layer">Sources of Control</text>
  <rect x="36" y="154" width="888" height="44" fill="${C.panelAlt}" stroke="${C.line}"/>
  <text x="52" y="181" class="policy-table-head">Control</text>
  <text x="298" y="181" class="policy-table-head">Set By</text>
  <text x="502" y="181" class="policy-table-head">What It Does Now</text>
  <text x="714" y="181" class="policy-table-head">How It Changes</text>
  ${controlRow(198, C.panel, "Personal Risk Policy", "trader", "advice / shadow", "new policy version")}
  ${controlRow(252, C.greenSoft, "Protection & Opportunity", "project or trader", "advisory output", "reviewed policy file")}
  ${controlRow(306, C.panel, "Runtime Settings", "trader", "product behavior", "allowlisted setting")}
  ${controlRow(360, C.blueSoft, "Analytical Models", "maintainer", "analysis context", "reviewed release")}
  ${controlRow(414, C.amberSoft, "Broker Safety Controls", "maintainer", "can block submission", "explicit guardrail change")}
  <text x="36" y="490" class="policy-table-sub">A built-in default is software-provided, not human approval. Settings are not policies.</text>

  <text x="36" y="536" class="layer">Two Separate Paths</text>
  ${flowBox(36, 564, 210, 128, "Policy + Evidence", ["risk boundary or model", "current facts + data quality"], C.blue)}
  ${flowBox(36, 744, 210, 128, "Human Authorization", ["one specific transaction", "never inferred from a preview"], C.amber, C.amberSoft)}

  <rect x="280" y="526" width="400" height="386" rx="18" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.5"/>
  <path d="M280 566h400v-22a18 18 0 0 0 -18 -18h-364a18 18 0 0 0 -18 18z" fill="${C.greenSoft}"/>
  <text x="300" y="552" class="boundary" style="fill:${C.greenDark}">Owned by the Canary Daemon</text>
  <text x="304" y="604" class="layer">Policy Advice</text>
  ${flowBox(304, 622, 152, 128, "Check", ["missing evidence", "stays unknown"], C.green, C.terminal, true)}
  ${flowBox(492, 622, 164, 128, "Decision Record", ["status + reasons", "saved locally"], C.slate)}
  <text x="304" y="790" class="layer">Broker Submission</text>
  ${flowBox(380, 806, 200, 94, "Safety Gates", ["account · route · freeze", "preview token · journal"], C.amber, C.amberSoft)}

  ${flowBox(714, 564, 210, 128, "Read-Only Views", ["CLI · app · MCP", "show the same result"], C.slate)}
  ${flowBox(714, 744, 210, 92, "IBKR Broker", ["execution venue"], C.greenDark)}
  ${flowBox(714, 856, 210, 92, "Broker Evidence", ["confirmation · statement"], C.blue)}

  ${line("M246 628H270V686H304", "blue", { dashed: true })}
  ${line("M456 686H492", "slate")}
  ${line("M656 686H714", "slate")}
  ${line("M246 808H272V853H380", "amber", { dotted: true })}
  ${line("M580 853H690V790H714", "amber", { dotted: true })}
  ${line("M819 836V856", "blue", { dashed: true })}

  <text x="924" y="982" text-anchor="end" class="footnote">Personal policy advises today. Broker records establish execution.</text>
  `;

  return svgFrame({
    width: 960,
    height: 1002,
    title: "Who Controls What (Canary)",
    description: "A compact matrix separates trader-authored policy, built-in or custom advisory policy, runtime settings, reviewed models, and broker safety controls. Below it, the daemon keeps policy advice separate from the human-authorized broker path, while broker records remain the evidence of execution.",
    body,
    extraStyles: policyReadableStyles,
  });
}

function storageOverview() {
  const storeModule = (x, y, width, iconName, title, sub) => `<g role="group" aria-label="${esc(`${title}. ${sub}`)}">
    <rect x="${x}" y="${y}" width="${width}" height="84" rx="10" fill="${C.terminal2}"/>
    ${icon(iconName, x + 14, y + 16, 24, C.textOnDark, 1.8)}
    <text x="${x + 50}" y="${y + 28}" class="module-title">${esc(title)}</text>
    <text x="${x + 14}" y="${y + 61}" class="module-sub">${esc(sub)}</text>
  </g>`;

  const body = `
  ${header("What the Daemon Keeps—and Where", "Inputs enter from the left; the daemon stores durable state; supported readers receive structured results.")}

  <text x="36" y="138" class="layer">1 · OWNED OUTSIDE SQLITE</text>
  <text x="726" y="138" class="layer">3 · SUPPORTED READERS</text>

  ${policyCard({ x: 36, y: 164, width: 210, height: 116, iconName: "fileText", color: C.greenDark, title: "Human TOML", lines: ["config · policy", "approved limits"], format: "people edit" })}
  ${policyCard({ x: 36, y: 300, width: 210, height: 132, iconName: "worldDownload", color: C.blue, title: "Original evidence", lines: ["Flex XML · broker data", "source observations"], format: "measured outside" })}
  ${policyCard({ x: 36, y: 452, width: 210, height: 116, iconName: "lock", color: C.amber, title: "Daemon secrets", lines: ["Flex token", "preview signer"], format: "private files" })}
  ${policyCard({ x: 36, y: 588, width: 210, height: 132, iconName: "mobileCode", color: C.slate, title: "App identity", lines: ["device grants · push", "relay credentials"], format: "separate app state" })}

  <rect x="276" y="118" width="420" height="620" rx="18" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.5"/>
  <path d="M276 158h420v-22a18 18 0 0 0 -18 -18h-384a18 18 0 0 0 -18 18z" fill="${C.greenSoft}"/>
  <text x="296" y="144" class="boundary" style="fill:${C.greenDark}">2 · THE DAEMON STORES AND SERVES</text>

  ${iconTile("serverCog", 296, 178, C.green, 44)}
  <text x="354" y="196" class="node-title">canary daemon</text>
  <text x="354" y="218" class="card-sub">validates · commits · serves structured results</text>

  <rect x="296" y="248" width="380" height="322" rx="16" fill="${C.terminal}"/>
  ${icon("database", 316, 268, 30, C.green, 1.8)}
  <text x="360" y="286" style="fill:#ffffff;font-size:20px;font-weight:700">daemon.db</text>
  <text x="316" y="316" class="module-sub">SQLite · WAL · transactional state and evidence</text>
  ${storeModule(316, 338, 166, "fileText", "Current state", "settings · risk · views")}
  ${storeModule(490, 338, 166, "route", "Events", "local decisions · lifecycle")}
  ${storeModule(316, 442, 166, "worldDownload", "Observations", "measured evidence")}
  ${storeModule(490, 442, 166, "shieldCheck", "Order safety", "routes · tokens · ID floors")}

  <rect x="296" y="592" width="380" height="118" rx="12" fill="${C.amberSoft}" stroke="${C.amber}" stroke-dasharray="7 5"/>
  ${icon("database", 316, 612, 26, C.amber, 1.8)}
  <text x="356" y="624" class="primer-title">Recovery is a separate boundary</text>
  <text x="316" y="654" class="card-sub"><tspan x="316">daemon.db.head · verified backups</tspan><tspan x="316" dy="20">sealed legacy artifacts</tspan></text>

  ${policyCard({ x: 726, y: 164, width: 198, height: 140, iconName: "exchange", color: C.slate, title: "CLI · app · MCP", lines: ["structured RPC", "no direct DB access"], format: "product API" })}
  ${policyCard({ x: 726, y: 332, width: 198, height: 140, iconName: "browser", color: C.blue, title: "Analytics", lines: ["bounded query", "or versioned export"], format: "never live-file SQL" })}
  ${policyCard({ x: 726, y: 500, width: 198, height: 140, iconName: "databaseImport", color: C.amber, title: "Offline review", lines: ["stopped daemon", "or verified backup"], format: "read-only" })}

  ${line("M252 222H270", "green")}
  ${line("M252 366H270", "blue", { dashed: true })}
  ${line("M252 510H270", "amber", { dotted: true })}
  ${line("M702 234H720", "slate")}
  ${line("M702 402H720", "blue", { dashed: true })}
  ${line("M702 570H720", "amber", { dotted: true })}

  <text x="924" y="780" text-anchor="end" class="footnote">app identity has its own owner · original broker evidence stays outside SQLite</text>
  `;

  return svgFrame({
    width: 960,
    height: 804,
    title: "What the Daemon Keeps and Where (Canary)",
    description: "Human-authored TOML, original broker and source evidence, daemon secrets, and app identity enter from their own locations. The daemon alone writes daemon.db, product surfaces use structured access, and recovery artifacts keep a separate lifecycle.",
    body,
    extraStyles: storageOverviewStyles,
  });
}

function dbTable({ x, y, width, title, rows, accent = C.green, fill = C.panel }) {
  const height = 52 + rows.length * 19;
  const content = rows.map((row, index) => {
    const [key, field] = row;
    const yy = y + 50 + index * 19;
    return `<text x="${x + 12}" y="${yy}" class="er-key">${esc(key)}</text><text x="${x + 56}" y="${yy}" class="er-field">${esc(field)}</text>`;
  }).join("");
  return `<g role="group" data-table="${esc(title)}" aria-label="Table ${esc(title)}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="8" fill="${fill}" stroke="${accent}" stroke-width="1.1"/>
    <path d="M${x} ${y + 34}h${width}" stroke="${accent}" stroke-width="1.1"/>
    <text x="${x + 12}" y="${y + 23}" class="er-title">${esc(title)}</text>
    ${content}
  </g>`;
}

function erFK({ parent, child, d, parentLabel = "", childLabel = "", parentX = 0, parentY = 0, childX = 0, childY = 0 }) {
  return `<g data-fk="${esc(child)}->${esc(parent)}">
    <path d="${d}" fill="none" stroke="${C.slate}" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
    ${parentLabel ? `<text x="${parentX}" y="${parentY}" class="er-cardinality">${esc(parentLabel)}</text>` : ""}
    ${childLabel ? `<text x="${childX}" y="${childY}" class="er-cardinality">${esc(childLabel)}</text>` : ""}
  </g>`;
}

function erSemantic(d, label, labelX, labelY) {
  return `<g><path d="${d}" fill="none" stroke="${C.blue}" stroke-width="1.4" stroke-dasharray="7 6" stroke-linecap="round" stroke-linejoin="round"/><text x="${labelX}" y="${labelY}" class="er-semantic">${esc(label)}</text></g>`;
}

// coreStoreSchemaInventory reports the table and foreign-key inventory a
// daemon.db carries once the whole migration plan has been applied, which is
function coreStoreSchemaInventory() {
  const source = fs.readFileSync(path.join(repoRoot, "internal/daemon/corestore/schema.go"), "utf8");
  let tables = [];
  let foreignKeys = [];
  const tablePattern = /`CREATE TABLE\s+([a-z_]+)\s+\(([\s\S]*?)\n\) STRICT`/g;
  for (const match of source.matchAll(tablePattern)) {
    const child = match[1];
    tables.push(child);
    for (const reference of match[2].matchAll(/REFERENCES\s+([a-z_]+)\s*\(/g)) {
      foreignKeys.push(`${child}->${reference[1]}`);
    }
  }
  const renamePattern = /`ALTER TABLE\s+([a-z_]+)\s+RENAME TO\s+([a-z_]+)`/g;
  for (const [, from, to] of source.matchAll(renamePattern)) {
    tables = tables.map((table) => (table === from ? to : table));
    foreignKeys = foreignKeys.map((fk) =>
      fk
        .split("->")
        .map((side) => (side === from ? to : side))
        .join("->"),
    );
  }
  const versions = [...source.matchAll(/^\s*version:\s*(\d+),$/gm)].map((match) => Number(match[1]));
  return {
    version: versions.length ? Math.max(...versions) : 1,
    tables: tables.sort(),
    foreignKeys: foreignKeys.sort(),
  };
}

function validateSQLiteER(svg) {
  const expected = coreStoreSchemaInventory();
  const rendered = {
    tables: [...svg.matchAll(/data-table="([a-z_]+)"/g)].map((match) => match[1]).sort(),
    foreignKeys: [...svg.matchAll(/data-fk="([a-z_]+->[a-z_]+)"/g)].map((match) => match[1]).sort(),
  };
  const compare = (name, actual, wanted) => {
    if (JSON.stringify(actual) !== JSON.stringify(wanted)) {
      throw new Error(`sqlite ER ${name} drift: rendered=${actual.join(",")} schema=${wanted.join(",")}`);
    }
  };
  compare("tables", rendered.tables, expected.tables);
  compare("foreign keys", rendered.foreignKeys, expected.foreignKeys);
}

function sqliteDataModel() {
  const schema = coreStoreSchemaInventory();
  const body = `
  ${header("daemon.db: How the Tables Relate", "Solid lines are SQLite foreign keys. Dashed lines are relationships enforced by the daemon.")}

  ${legendItem(610, 40, "slate", "Declared foreign key")}
  ${legendItem(790, 40, "blue", "Application convention", { dashed: true })}
  <text x="610" y="68" class="legend">Cardinality: 1 · 0..1 · 0..*</text>

  <text x="36" y="138" class="layer">STANDALONE STATE, EVIDENCE &amp; STORE CONTROL</text>
  ${dbTable({ x: 36, y: 156, width: 170, title: "store_meta", rows: [["PK", "singleton"], ["", "head counters"]], accent: C.amber })}
  ${dbTable({ x: 218, y: 156, width: 170, title: "schema_migrations", rows: [["PK", "version"], ["", "checksum"]], accent: C.slate })}
  ${dbTable({ x: 400, y: 156, width: 170, title: "legacy_imports", rows: [["PK", "scope/src/fp"], ["UQ", "scope/source"]], accent: C.slate })}
  ${dbTable({ x: 582, y: 156, width: 170, title: "state_documents", rows: [["PK", "scope/kind"], ["", "rev · JSON"], ["", "sha256"]], accent: C.green })}
  ${dbTable({ x: 764, y: 156, width: 170, title: "observations", rows: [["PK", "id"], ["", "source/kind"], ["", "payload/elig"]], accent: C.blue })}

  <text x="36" y="326" class="layer">CANONICAL EVENT AND OPTIONAL SEARCH PROJECTIONS</text>
  ${dbTable({ x: 36, y: 354, width: 270, title: "event_log", rows: [["PK", "event_seq"], ["UQ", "scope + event_key"], ["", "type · action · origin"], ["", "payload JSON · sha256"]], accent: C.slate })}
  ${dbTable({ x: 350, y: 354, width: 270, title: "regime_decisions", rows: [["PK/FK", "event_seq"], ["", "stage · verdict"]], accent: C.slate })}
  ${dbTable({ x: 654, y: 354, width: 270, title: "rule_transitions", rows: [["PK/FK", "event_seq"], ["", "rule · status"]], accent: C.slate })}
  ${dbTable({ x: 350, y: 480, width: 270, title: "stress_transitions", rows: [["PK/FK", "event_seq"], ["", "action · health"]], accent: C.slate })}
  ${dbTable({ x: 654, y: 480, width: 270, title: "capital_events", rows: [["PK/FK", "event_seq"], ["", "kind · amount"]], accent: C.slate })}
  ${dbTable({ x: 350, y: 606, width: 270, title: "risk_policy_events", rows: [["PK/FK", "event_seq"], ["", "policy identity"]], accent: C.slate })}
  ${dbTable({ x: 654, y: 606, width: 270, title: "proposal_outcomes", rows: [["PK/FK", "event_seq"], ["", "proposal · state"]], accent: C.slate })}
  ${dbTable({ x: 350, y: 732, width: 270, title: "order_events", rows: [["PK/FK", "event_seq"], ["FK", "scope_key"]], accent: C.slate })}
  ${dbTable({ x: 654, y: 732, width: 270, title: "regime_indicators", rows: [["PK", "decision + indicator"], ["FK", "decision_event_seq"], ["", "value · status"]], accent: C.slate })}

  ${erFK({ parent: "event_log", child: "regime_decisions", d: "M306 420H326V406H350", parentLabel: "1", childLabel: "0..1", parentX: 312, parentY: 410, childX: 330, childY: 398 })}
  ${erFK({ parent: "event_log", child: "stress_transitions", d: "M306 420H326V532H350", childLabel: "0..1", childX: 330, childY: 524 })}
  ${erFK({ parent: "event_log", child: "risk_policy_events", d: "M306 420H326V658H350", childLabel: "0..1", childX: 330, childY: 650 })}
  ${erFK({ parent: "event_log", child: "order_events", d: "M306 420H326V770H350", childLabel: "0..1", childX: 330, childY: 762 })}
  ${erFK({ parent: "event_log", child: "rule_transitions", d: "M171 354V336H940V406H924", childLabel: "0..1", childX: 878, childY: 398 })}
  ${erFK({ parent: "event_log", child: "capital_events", d: "M171 354V336H940V532H924", childLabel: "0..1", childX: 878, childY: 524 })}
  ${erFK({ parent: "event_log", child: "proposal_outcomes", d: "M171 354V336H940V658H924", childLabel: "0..1", childX: 878, childY: 650 })}
  ${erFK({ parent: "regime_decisions", child: "regime_indicators", d: "M620 406H634V795H654", parentLabel: "1", childLabel: "0..*", parentX: 624, parentY: 426, childX: 602, childY: 788 })}

  <rect x="36" y="548" width="270" height="250" rx="14" fill="${C.blueSoft}" stroke="${C.blue}" stroke-dasharray="7 6"/>
  <text x="56" y="578" class="boundary" style="fill:${C.blue}"><tspan x="56">APPLICATION CONVENTIONS</tspan><tspan x="56" dy="18">NOT FOREIGN KEYS</tspan></text>
  <text x="56" y="634" class="er-note"><tspan x="56">• current state may commit</tspan><tspan x="56" dy="22">  beside an event or observation</tspan><tspan x="56" dy="32">• one projection family per event</tspan><tspan x="56" dy="22">  is a Go-writer rule</tspan><tspan x="56" dy="32">• shared scope names do not</tspan><tspan x="56" dy="22">  create a relationship</tspan></text>

  <text x="350" y="886" class="layer">BROKER ROUTE AND IRREVERSIBLE SAFETY</text>
  ${dbTable({ x: 36, y: 908, width: 270, title: "broker_scopes", rows: [["PK", "scope_key"], ["UQ", "binding_sha256"], ["", "endpoint · client · account"]], accent: C.greenDark })}
  ${dbTable({ x: 350, y: 908, width: 270, title: "consumed_preview_tokens", rows: [["PK", "token_digest"], ["FK", "scope_key"]], accent: C.amber })}
  ${dbTable({ x: 654, y: 908, width: 270, title: "order_id_floors", rows: [["PK", "floor_scope + scope"], ["", "floor never decreases"]], accent: C.amber })}

  ${erFK({ parent: "broker_scopes", child: "consumed_preview_tokens", d: "M306 960H350", parentLabel: "1", childLabel: "0..*", parentX: 312, parentY: 950, childX: 310, childY: 982 })}
  ${erFK({ parent: "broker_scopes", child: "order_events", d: "M171 908V876H336V806H350", parentLabel: "1", childLabel: "0..*", parentX: 180, parentY: 896, childX: 304, childY: 818 })}
  ${erSemantic("M654 984H638V1050H171V1034", "broker association enforced by code; no FK", 390, 1072)}

  <rect x="36" y="1100" width="888" height="330" rx="16" fill="${C.panel}" stroke="${C.greenLine}"/>
  <text x="56" y="1132" class="layer">BROKER STATEMENTS · CURRENT VIEW AND IMMUTABLE RESTATEMENTS</text>
  ${policyCard({ x: 56, y: 1152, width: 190, height: 128, iconName: "fileText", color: C.blue, title: "Original Flex XML", lines: ["broker evidence"], format: "outside SQLite" })}
  ${dbTable({ x: 278, y: 1152, width: 280, title: "statement_files", rows: [["PK", "scope + file_key"], ["UQ", "+ current sha256"]], accent: C.green })}
  ${dbTable({ x: 590, y: 1152, width: 314, title: "statement_equity_days", rows: [["PK", "equity_day_id"], ["UQ", "scope + account + day"], ["FK", "current file + sha256"]], accent: C.green })}
  ${dbTable({ x: 278, y: 1300, width: 280, title: "statement_file_versions", rows: [["PK", "scope + file + sha256"], ["", "immutable file version"]], accent: C.slate })}
  ${dbTable({ x: 590, y: 1300, width: 314, title: "statement_equity_day_versions", rows: [["PK", "equity_version_id"], ["FK", "file version + sha256"]], accent: C.slate })}

  ${erFK({ parent: "statement_files", child: "statement_equity_days", d: "M558 1204H590", parentLabel: "1", childLabel: "0..*", parentX: 562, parentY: 1194, childX: 558, childY: 1226 })}
  ${erFK({ parent: "statement_file_versions", child: "statement_equity_day_versions", d: "M558 1352H590", parentLabel: "1", childLabel: "0..*", parentX: 562, parentY: 1342, childX: 558, childY: 1374 })}
  ${erSemantic("M246 1216H272", "parsed from", 174, 1206)}
  ${erSemantic("M246 1232H260V1352H272", "retained versions", 96, 1344)}
  ${erSemantic("M418 1256V1294", "written together · no FK", 286, 1282)}
  ${erSemantic("M747 1278V1294", "", 0, 0)}

  <text x="924" y="1466" text-anchor="end" class="footnote">schema v${schema.version} · ${schema.tables.length} tables · inventory checked against internal/daemon/corestore/schema.go</text>
  `;

  const svg = svgFrame({
    width: 960,
    height: 1490,
    title: "How daemon.db Tables Relate (Canary)",
    description: `A physical entity relationship diagram for SQLite schema version ${schema.version}. Solid lines represent declared foreign keys with cardinalities. Dashed lines identify application-level relationships that are not enforced by SQLite. Standalone state, observation, migration, import, and order-floor tables have no invented relationships.`,
    body,
    extraStyles: storageERStyles,
  });
  validateSQLiteER(svg);
  return svg;
}

function stepCard({ x, y, width, number, title, lines, color = C.slate }) {
  const height = 104;
  return `<g role="group" aria-label="Step ${number}. ${esc([title, ...lines].join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="12" fill="${C.panel}" stroke="${color}" stroke-width="1.2"/>
    <circle cx="${x + 24}" cy="${y + 25}" r="15" fill="${color}"/>
    <text x="${x + 24}" y="${y + 29}" text-anchor="middle" class="step-no">${number}</text>
    <text x="${x + 48}" y="${y + 29}" class="step-title">${esc(title)}</text>
    ${textLines(x + 16, y + 58, lines, "step-sub", 15)}
  </g>`;
}

function sensorBox({ x, y, width, height, title, lines, color = C.slate, iconName = "cpu", fill = C.panel, dark = false }) {
  const titleFill = dark ? "#ffffff" : C.ink;
  const bodyClass = dark ? "node-sub on-dark" : "card-sub";
  return `<g role="group" aria-label="${esc([title, ...lines].join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="13" fill="${fill}" stroke="${color}" stroke-width="1.3"/>
    ${iconTile(iconName, x + 16, y + 16, color, 36)}
    <text x="${x + 64}" y="${y + 38}" class="card-title" style="fill:${titleFill}">${esc(title)}</text>
    ${textLines(x + 18, y + 76, lines, bodyClass, 18)}
  </g>`;
}

function sensorAuthorityPipeline() {
  const body = `
  ${header("Sensor Measurement and Authority", "Observed facts flow through daemon-owned sensors before any downstream alert view.")}

  <text x="36" y="142" class="layer">1 · OBSERVED SOURCES</text>
  <text x="352" y="142" class="layer">2 · PRIMARY SENSORS</text>
  <text x="710" y="142" class="layer">3 · DEPENDENT DECISIONS</text>
  <text x="1114" y="142" class="layer">4 · DOWNSTREAM CONSUMERS</text>

  <rect x="24" y="158" width="284" height="570" rx="17" fill="${C.panelAlt}" stroke="${C.line}"/>
  ${sensorBox({ x: 42, y: 184, width: 248, height: 116, title: "TWS / Gateway", lines: ["account · positions · tape", "quotes · chains · OI"], color: C.blue, iconName: "plugConnected" })}
  ${sensorBox({ x: 42, y: 326, width: 248, height: 116, title: "Official & Public", lines: ["Nasdaq · FRED · Cboe", "calendars · membership"], color: C.greenDark, iconName: "worldDownload" })}
  ${sensorBox({ x: 42, y: 468, width: 248, height: 116, title: "IBKR FTP", lines: ["global borrow availability", "annualized fee evidence"], color: C.green, iconName: "databaseImport" })}
  ${sensorBox({ x: 42, y: 610, width: 248, height: 94, title: "daemon.db", lines: ["last-good + observations"], color: C.slate, iconName: "database" })}

  <rect x="334" y="158" width="334" height="570" rx="17" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.4"/>
  ${sensorBox({ x: 352, y: 184, width: 298, height: 132, title: "Gamma", lines: ["SPX canonical · SPY context", "0DTE · 1–7 · term"], color: C.blue, iconName: "cpu" })}
  ${sensorBox({ x: 352, y: 342, width: 298, height: 132, title: "Market Series", lines: ["vol · credit · funding · FX", "breadth + source health"], color: C.greenDark, iconName: "worldDownload" })}
  ${sensorBox({ x: 352, y: 500, width: 298, height: 132, title: "Market Events", lines: ["Reg SHO · halts · borrow", "held-name typed context"], color: C.green, iconName: "bell" })}
  ${sensorBox({ x: 352, y: 650, width: 298, height: 58, title: "Portfolio State", lines: [], color: C.blue, iconName: "user" })}

  <rect x="692" y="158" width="368" height="570" rx="17" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.4"/>
  ${sensorBox({ x: 710, y: 184, width: 332, height: 126, title: "Regime", lines: ["8 rows → 6 clusters", "5-minute last-good authority"], color: C.green, iconName: "serverCog", fill: C.terminal, dark: true })}
  ${sensorBox({ x: 710, y: 352, width: 332, height: 126, title: "Stress", lines: ["Regime + held portfolio", "one-minute fail-closed view"], color: C.amber, iconName: "shieldCheck" })}
  ${sensorBox({ x: 710, y: 520, width: 332, height: 126, title: "Rulebook", lines: ["account + book + events", "one-minute advisory checks"], color: C.slate, iconName: "fileText" })}

  <rect x="1096" y="158" width="320" height="570" rx="17" fill="${C.panelAlt}" stroke="${C.line}"/>
  ${sensorBox({ x: 1114, y: 238, width: 284, height: 136, title: "Alerts", lines: ["consumes typed decisions", "delivery is a separate claim"], color: C.amber, iconName: "bell" })}
  ${sensorBox({ x: 1114, y: 420, width: 284, height: 136, title: "Read-only Views", lines: ["CLI · MCP · app · web", "render; never re-decide"], color: C.slate, iconName: "browser" })}

  ${line("M290 242H352", "blue")}
  ${line("M290 384H352", "green")}
  ${line("M290 526H352", "green")}
  ${line("M290 657H334", "slate", { dashed: true, both: true })}
  ${line("M290 270H320V679H352", "blue", { dashed: true })}
  ${line("M650 250H680V247H710", "blue")}
  ${line("M650 408H680V247H710", "green")}
  ${line("M876 310V352", "green")}
  ${line("M650 566H680V415H710", "green")}
  ${line("M650 679H684V415H710", "blue", { dashed: true })}
  ${line("M650 679H694V583H710", "blue", { dashed: true })}
  ${line("M1042 247H1068V583H1042", "green", { dashed: true })}
  ${line("M1042 247H1076V306H1114", "green")}
  ${line("M1042 415H1076V306H1114", "amber")}
  ${line("M1042 583H1076V306H1114", "slate")}
  ${line("M1042 247H1082V488H1114", "slate", { dashed: true })}
  ${line("M1042 415H1082V488H1114", "slate", { dashed: true })}
  ${line("M1042 583H1082V488H1114", "slate", { dashed: true })}

  <text x="1404" y="758" text-anchor="end" class="footnote">measurement is not policy · sensor state is not proof of delivery</text>
  `;

  return svgFrame({
    width: 1440,
    height: 780,
    title: "Sensor Measurement and Authority (Canary)",
    description: "Broker and public observations enter daemon-owned Gamma, market-series, and market-event sensors. Regime consumes Gamma and market series; Stress consumes Regime, portfolio, and held-name events; Rulebook consumes current desk evidence. Alerts and read-only surfaces consume typed results without recreating authority.",
    body,
    extraStyles: policyReadableStyles,
  });
}

function sensorFreshnessTimeline() {
  const body = `
  ${header("Freshness, Last-good, and Gaps", "A schedule says when a replacement is due; health says whether the old observation may still be used.")}

  ${legendItem(962, 42, "green", "usable authority")}
  ${legendItem(1132, 42, "blue", "refresh work", { dashed: true })}
  ${legendItem(962, 66, "amber", "context / gap")}
  ${legendItem(1132, 66, "slate", "time boundary", { dotted: true })}

  <text x="42" y="150" class="layer">SOURCE IS DUE · PROACTIVE REFRESH</text>
  <line x1="166" y1="244" x2="1370" y2="244" stroke="${C.line}" stroke-width="2"/>
  <text x="42" y="249" class="matrix-head">Due source</text>

  <rect x="166" y="184" width="390" height="88" rx="12" fill="${C.greenSoft}" stroke="${C.green}"/>
  <text x="186" y="216" class="card-title">Current last-good</text>
  <text x="186" y="242" class="card-sub">published value remains authoritative</text>
  <text x="186" y="260" class="mono-small">as_of fixed · health=current</text>

  <rect x="556" y="184" width="250" height="88" rx="12" fill="${C.blueSoft}" stroke="${C.blue}" stroke-dasharray="7 5"/>
  <text x="576" y="216" class="card-title">Refresh starts early</text>
  <text x="576" y="242" class="card-sub">old result stays served</text>
  <text x="576" y="260" class="mono-small">single flight · bounded timeout</text>

  <line x1="806" y1="158" x2="806" y2="326" stroke="${C.slate}" stroke-width="1.5" stroke-dasharray="2 5"/>
  <text x="806" y="146" text-anchor="middle" class="card-note">hard expiry</text>

  <path d="M806 214H850V176H900" fill="none" stroke="${C.green}" stroke-width="1.8" marker-end="url(#arrow-green)"/>
  <rect x="900" y="142" width="250" height="78" rx="12" fill="${C.greenSoft}" stroke="${C.green}"/>
  <text x="920" y="174" class="card-title">Success before expiry</text>
  <text x="920" y="198" class="card-sub">atomically publish replacement</text>

  <path d="M806 252H850V294H900" fill="none" stroke="${C.amber}" stroke-width="1.8" marker-end="url(#arrow-amber)"/>
  <rect x="900" y="258" width="250" height="78" rx="12" fill="${C.amberSoft}" stroke="${C.amber}"/>
  <text x="920" y="290" class="card-title">Failure at expiry</text>
  <text x="920" y="314" class="card-sub">old value becomes stale</text>

  ${line("M1150 297H1190", "amber")}
  <rect x="1190" y="258" width="180" height="78" rx="12" fill="${C.panel}" stroke="${C.amber}"/>
  <text x="1210" y="290" class="card-title">Fail closed</text>
  <text x="1210" y="314" class="card-sub">dependent readiness blocks</text>

  <text x="42" y="412" class="layer">SOURCE IS NOT DUE · SESSION-AWARE CARRY</text>
  <line x1="166" y1="506" x2="1370" y2="506" stroke="${C.line}" stroke-width="2"/>
  <text x="42" y="511" class="matrix-head">Closed window</text>

  <rect x="166" y="446" width="278" height="88" rx="12" fill="${C.greenSoft}" stroke="${C.green}"/>
  <text x="186" y="478" class="card-title">Completed-session result</text>
  <text x="186" y="504" class="card-sub">valid last-good observation</text>
  <text x="186" y="522" class="mono-small">session closes</text>

  <rect x="444" y="446" width="448" height="88" rx="12" fill="${C.amberSoft}" stroke="${C.amber}"/>
  <text x="464" y="478" class="card-title">Typed not_due context</text>
  <text x="464" y="504" class="card-sub">no newer observation should exist · no automatic churn</text>
  <text x="464" y="522" class="mono-small">visible, but not confirmation-eligible</text>

  <line x1="892" y1="420" x2="892" y2="590" stroke="${C.slate}" stroke-width="1.5" stroke-dasharray="2 5"/>
  <text x="892" y="408" text-anchor="middle" class="card-note">next source window opens</text>

  <rect x="892" y="446" width="248" height="88" rx="12" fill="${C.blueSoft}" stroke="${C.blue}" stroke-dasharray="7 5"/>
  <text x="912" y="478" class="card-title">Replacement is due</text>
  <text x="912" y="504" class="card-sub">refresh current-session evidence</text>
  <text x="912" y="522" class="mono-small">old session no longer not_due</text>

  ${line("M1140 490H1180", "amber")}
  <rect x="1180" y="446" width="190" height="88" rx="12" fill="${C.panel}" stroke="${C.amber}"/>
  <text x="1200" y="478" class="card-title">Overdue gap</text>
  <text x="1200" y="504" class="card-sub">undefined, never warning</text>
  <text x="1200" y="522" class="mono-small">retry state explicit</text>

  <rect x="166" y="604" width="1204" height="82" rx="14" fill="${C.panel}" stroke="${C.line}"/>
  ${iconTile("shieldCheck", 186, 624, C.slate, 38)}
  <text x="240" y="640" class="card-title">One rule across every sensor</text>
  <text x="240" y="666" class="card-sub">Keep independently current evidence visible. Never let stale, partial, unavailable, or overdue required data become a pass or an early-warning signal.</text>

  <text x="1404" y="722" text-anchor="end" class="footnote">timestamps never move unless a replacement is actually accepted</text>
  `;

  return svgFrame({
    width: 1440,
    height: 744,
    title: "Freshness, Last-good, and Gaps (Canary)",
    description: "The due-source lane shows a current last-good value, a proactive refresh before hard expiry, successful atomic replacement, or stale fail-closed behavior after failure. The closed-window lane shows completed-session context as typed not-due until the next source window opens, after which a missing replacement is overdue and blocks dependent readiness.",
    body,
    extraStyles: policyReadableStyles,
  });
}

const cleanGeneratedSVG = (svg) => svg.replace(/^[ \t]+$/gm, "");

const outputs = new Map([
  ["system-architecture.svg", systemArchitecture()],
  ["data-and-persistence.svg", persistenceArchitecture()],
  ["policy-lifecycle.svg", cleanGeneratedSVG(policyPrimer())],
  ["policy-authority.svg", cleanGeneratedSVG(policyArchitecture())],
  ["storage-overview.svg", cleanGeneratedSVG(storageOverview())],
  ["sqlite-data-model.svg", cleanGeneratedSVG(sqliteDataModel())],
  ["sensor-authority-pipeline.svg", cleanGeneratedSVG(sensorAuthorityPipeline())],
  ["sensor-freshness-timeline.svg", cleanGeneratedSVG(sensorFreshnessTimeline())],
]);

if (process.argv.includes("--check")) {
  const stale = [];
  for (const [name, expected] of outputs) {
    const output = path.join(outDir, name);
    if (!fs.existsSync(output) || fs.readFileSync(output, "utf8") !== expected) stale.push(name);
  }
  if (stale.length) {
    console.error(`diagram check: stale output: ${stale.join(", ")}; run node scripts/render-architecture.mjs`);
    process.exitCode = 1;
  } else {
    console.log(`diagram check: ${outputs.size} SVG diagram(s) match the renderer`);
  }
} else {
  for (const [name, content] of outputs) fs.writeFileSync(path.join(outDir, name), content);
  console.log(`rendered ${outputs.size} architecture diagrams`);
}
