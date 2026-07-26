#!/usr/bin/env node
// Regenerate the published CLI screenshots from cmd/_preview fixtures only.

import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { promisify } from "node:util";

import { launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

const execFileAsync = promisify(execFile);
const ACCOUNT_ID = /\bD?U\d{6,9}\b/g;
const FIXTURE_ACCOUNT_ID = "DU0000000";
const DEFAULT_COLOR = "#e6edf7";

const SCREENS = [
  { name: "regime", title: "canary regime", width: 1280 },
  { name: "stress", title: "canary stress", width: 1280 },
  { name: "chain", title: "canary chain SPY", width: 1080 },
  { name: "positions", title: "canary positions --by underlying", width: 1280 },
];

export function ansiToHTML(input) {
  const state = { bold: false, dim: false, color: null };
  const sgr = /\x1b\[([0-9;]*)m/g;
  let html = "";
  let offset = 0;

  for (const match of input.matchAll(sgr)) {
    html += styledHTML(input.slice(offset, match.index), state);
    applySGR(state, match[1]);
    offset = match.index + match[0].length;
  }
  return html + styledHTML(input.slice(offset), state);
}

function applySGR(state, sequence) {
  const codes = sequence === "" ? [0] : sequence.split(";").map(Number);
  for (let i = 0; i < codes.length; i++) {
    const code = codes[i];
    switch (code) {
      case 0:
        state.bold = false;
        state.dim = false;
        state.color = null;
        break;
      case 1:
        state.bold = true;
        break;
      case 2:
        state.dim = true;
        break;
      case 22:
        state.bold = false;
        state.dim = false;
        break;
      case 31:
        state.color = "#ff6b7a";
        break;
      case 32:
        state.color = "#66e3bb";
        break;
      case 33:
        state.color = "#ffd866";
        break;
      case 36:
        state.color = "#56d6e8";
        break;
      case 39:
        state.color = null;
        break;
      case 38:
        if (codes[i + 1] === 5 && Number.isInteger(codes[i + 2])) {
          const color = xtermColor(codes[i + 2]);
          if (color) {
            state.color = color;
          }
          i += 2;
        } else if (codes[i + 1] === 2 && codes.slice(i + 2, i + 5).every(validByte)) {
          state.color = `rgb(${codes[i + 2]}, ${codes[i + 3]}, ${codes[i + 4]})`;
          i += 4;
        }
        break;
      default:
        // Unsupported SGR codes deliberately leave the current style intact.
        break;
    }
  }
}

function validByte(value) {
  return Number.isInteger(value) && value >= 0 && value <= 255;
}

function xtermColor(value) {
  if (!validByte(value)) {
    return null;
  }
  const base = [
    "#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
    "#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff",
  ];
  if (value < base.length) {
    return base[value];
  }
  if (value < 232) {
    const index = value - 16;
    const levels = [0, 95, 135, 175, 215, 255];
    const red = levels[Math.floor(index / 36)];
    const green = levels[Math.floor((index % 36) / 6)];
    const blue = levels[index % 6];
    return `rgb(${red}, ${green}, ${blue})`;
  }
  const gray = 8 + (value - 232) * 10;
  return `rgb(${gray}, ${gray}, ${gray})`;
}

function styledHTML(text, state) {
  if (!text) {
    return "";
  }
  const escaped = escapeHTML(text);
  const styles = [];
  if (state.bold) {
    styles.push("font-weight:700");
  }
  if (state.dim) {
    styles.push("opacity:0.62");
  }
  if (state.color) {
    styles.push(`color:${state.color}`);
  }
  return styles.length === 0 ? escaped : `<span style="${styles.join(";")}">${escaped}</span>`;
}

function escapeHTML(value) {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function selfTest() {
  assert.equal(ansiToHTML("plain line\n"), "plain line\n");
  assert.equal(
    ansiToHTML("\x1b[1mbold\x1b[22m plain"),
    '<span style="font-weight:700">bold</span> plain',
  );
  assert.equal(
    ansiToHTML("\x1b[2mdim\x1b[0m plain"),
    '<span style="opacity:0.62">dim</span> plain',
  );
  assert.equal(
    ansiToHTML("\x1b[31mred & <safe>\x1b[39m plain"),
    '<span style="color:#ff6b7a">red &amp; &lt;safe&gt;</span> plain',
  );
  assert.equal(
    ansiToHTML("\x1b[38;5;214morange\x1b[0m reset"),
    '<span style="color:rgb(255, 175, 0)">orange</span> reset',
  );
  console.log("cli-screenshots: ANSI parser self-test passed");
}

async function capturePreview(screen) {
  const { stdout } = await execFileAsync("go", ["run", "./cmd/_preview", screen.name], {
    encoding: "utf8",
    env: { ...process.env, GOPROXY: "off" },
    maxBuffer: 10 * 1024 * 1024,
  });
  return { ...screen, text: stdout };
}

function assertFixtureOnly(captures) {
  for (const capture of captures) {
    for (const match of capture.text.matchAll(ACCOUNT_ID)) {
      if (match[0] !== FIXTURE_ACCOUNT_ID) {
        throw new Error(`${capture.name}: account-id-shaped text found; aborting before writing screenshots`);
      }
    }
  }
}

function terminalPage(capture) {
  return `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <style>
    * { box-sizing: border-box; }
    html, body { margin: 0; width: ${capture.width}px; background: transparent; }
    body { color: ${DEFAULT_COLOR}; }
    #terminal {
      width: ${capture.width}px;
      overflow: hidden;
      background: #0e1626;
      border: 1px solid #2b405f;
      border-radius: 10px;
    }
    .titlebar {
      position: relative;
      height: 48px;
      background: #111d33;
    }
    .lights {
      position: absolute;
      top: 18px;
      left: 20px;
      display: flex;
      gap: 10px;
    }
    .light { width: 12px; height: 12px; border-radius: 50%; }
    .close { background: #ff6b7a; }
    .minimize { background: #ffd866; }
    .maximize { background: #66e3bb; }
    .title {
      position: absolute;
      top: 0;
      left: 50%;
      height: 48px;
      transform: translateX(-50%);
      display: flex;
      align-items: center;
      color: #91a0b8;
      font: 13px -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      white-space: nowrap;
    }
    pre {
      margin: 0;
      padding: 24px;
      color: ${DEFAULT_COLOR};
      font-family: "SF Mono", Menlo, Consolas, monospace;
      font-size: 13px;
      line-height: 1.4;
      white-space: pre;
      tab-size: 4;
      -webkit-font-smoothing: antialiased;
    }
  </style>
</head>
<body>
  <section id="terminal">
    <header class="titlebar">
      <div class="lights" aria-hidden="true">
        <i class="light close"></i><i class="light minimize"></i><i class="light maximize"></i>
      </div>
      <div class="title">${escapeHTML(capture.title)}</div>
    </header>
    <pre>${ansiToHTML(capture.text)}</pre>
  </section>
</body>
</html>`;
}

async function render(captures, outDir) {
  const playwright = loadPlaywright("cli-screenshots");
  const { browser } = await launchBrowser(playwright.chromium, "chromium", { headless: true });
  try {
    await fs.mkdir(outDir, { recursive: true });
    for (const capture of captures) {
      const context = await browser.newContext({
        viewport: { width: capture.width, height: 720 },
        deviceScaleFactor: 2,
      });
      try {
        const page = await context.newPage();
        await page.setContent(terminalPage(capture), { waitUntil: "load" });
        const out = path.join(outDir, `${capture.name}.png`);
        await page.locator("#terminal").screenshot({ path: out });
        console.log(`cli-screenshots: wrote ${out} (${capture.width}px CSS @2x)`);
      } finally {
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  if (args["self-test"]) {
    selfTest();
    return;
  }

  const captures = [];
  for (const screen of SCREENS) {
    captures.push(await capturePreview(screen));
  }
  assertFixtureOnly(captures);
  await render(captures, args["out-dir"] || "docs/social");
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (isMain) {
  await main();
}
