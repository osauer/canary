#!/usr/bin/env node
// Regenerate docs/social/social-preview.png from deterministic local inputs.
//
// The terminal image comes from scripts/cli-screenshots.mjs, which renders the
// synthetic cmd/_preview fixtures. This script adds only fixed product copy and
// writes the published image after all checks pass.

import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";

import { launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

const args = parseArgs(process.argv.slice(2));
const input = args.input || "docs/social/regime.png";
const output = args.output || "docs/social/social-preview.png";
const width = 1280;
const height = 640;

const terminalPNG = await fs.readFile(input);
const terminalDataURL = `data:image/png;base64,${terminalPNG.toString("base64")}`;
const playwright = loadPlaywright("social-preview");
const { browser } = await launchBrowser(playwright.chromium, "chromium", { headless: true });

try {
  const context = await browser.newContext({
    viewport: { width, height },
    deviceScaleFactor: 1,
  });
  try {
    const page = await context.newPage();
    await page.setContent(pageHTML(terminalDataURL), { waitUntil: "load" });
    await page.waitForFunction(() => {
      const image = document.querySelector("#terminal-shot");
      return image instanceof HTMLImageElement && image.complete && image.naturalWidth > 0;
    });

    const bodyText = await page.locator("body").innerText();
    assert.equal(/\bD?U\d{6,9}\b/.test(bodyText), false, "account-id-shaped text in social preview");
    assert.equal(/\bibkr\b/.test(bodyText), false, "retired lowercase product name in social preview");
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth), width);
    assert.equal(await page.evaluate(() => document.documentElement.scrollHeight), height);

    const png = await page.screenshot();
    await fs.mkdir(path.dirname(output), { recursive: true });
    await fs.writeFile(output, png);
    console.log(`social-preview: wrote ${output} (${width}x${height})`);
  } finally {
    await context.close();
  }
} finally {
  await browser.close();
}

function pageHTML(imageURL) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <style>
    * { box-sizing: border-box; }
    html, body {
      width: ${width}px;
      height: ${height}px;
      margin: 0;
      overflow: hidden;
      background: #fbfaf6;
      color: #101827;
      font-family: Inter, ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      -webkit-font-smoothing: antialiased;
    }
    body::before {
      content: "";
      position: absolute;
      inset: 0 0 auto;
      height: 12px;
      background: #087f6d;
    }
    main {
      display: grid;
      grid-template-columns: 52% 48%;
      align-items: center;
      width: 100%;
      height: 100%;
      padding: 56px 48px 48px 72px;
    }
    .copy { padding-right: 42px; }
    .brand {
      margin: 0 0 28px;
      color: #087f6d;
      font-size: 30px;
      font-weight: 800;
      letter-spacing: -0.02em;
    }
    h1 {
      max-width: 590px;
      margin: 0;
      font-size: 55px;
      line-height: 1.08;
      letter-spacing: -0.045em;
    }
    .lead {
      max-width: 560px;
      margin: 30px 0 0;
      color: #35425a;
      font-size: 25px;
      line-height: 1.38;
    }
    .boundary {
      margin: 34px 0 0;
      color: #087f6d;
      font-size: 19px;
      font-weight: 750;
      line-height: 1.45;
    }
    .terminal {
      position: relative;
      width: 100%;
      height: 280px;
      overflow: hidden;
      border: 1px solid #2b405f;
      border-radius: 12px;
      background: #0e1626;
      box-shadow: 0 18px 45px rgb(16 24 39 / 16%);
    }
    #terminal-shot {
      display: block;
      width: 100%;
      height: auto;
    }
  </style>
</head>
<body>
  <main>
    <section class="copy">
      <p class="brand">Canary</p>
      <h1>Interactive Brokers context, available locally.</h1>
      <p class="lead">One CLI and MCP server for account, market, and risk views through your own TWS or IB Gateway session.</p>
      <p class="boundary">Typed data quality · no MCP order-entry tools</p>
    </section>
    <figure class="terminal" aria-label="Synthetic Canary regime output">
      <img id="terminal-shot" src="${imageURL}" alt="">
    </figure>
  </main>
</body>
</html>`;
}
