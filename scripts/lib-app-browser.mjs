// lib-app-browser.mjs — helpers shared by the Playwright-driven app
// scripts (currently app-browser-smoke.mjs): CLI arg parsing,
// Playwright resolution across runtimes, and `canary app` pairing.

import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

export function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) {
      continue;
    }
    const [rawKey, inlineValue] = arg.slice(2).split("=", 2);
    if (inlineValue !== undefined) {
      out[rawKey] = inlineValue;
      continue;
    }
    if (i + 1 < argv.length && !argv[i + 1].startsWith("--")) {
      out[rawKey] = argv[++i];
      continue;
    }
    out[rawKey] = "true";
  }
  return out;
}

export function loadPlaywright(toolName) {
  const require = createRequire(import.meta.url);
  const errors = [];
  try {
    return require("playwright");
  } catch (err) {
    errors.push(`default resolution: ${String(err?.message || err)}`);
  }

  for (const moduleRoot of candidateModuleRoots()) {
    try {
      return require(path.join(moduleRoot, "playwright"));
    } catch (err) {
      errors.push(`${moduleRoot}: ${String(err?.message || err)}`);
    }
  }

  console.error(`${toolName}: Playwright is not installed for this Node runtime.`);
  console.error("Install it with `npm install playwright`, or run inside Codex where the bundled runtime can be discovered.");
  console.error(errors.join("\n"));
  process.exit(2);
}

export function assertBrowserLaunchAllowed({
  platform = process.platform,
  codexSandbox = process.env.CODEX_SANDBOX,
} = {}) {
  if (platform !== "darwin" || !codexSandbox) {
    return;
  }

  throw new Error([
    "Playwright cannot launch a macOS browser inside the Codex sandbox.",
    "AppKit aborts the browser during application registration instead of returning a normal permission error.",
    "Re-run this browser-launching command with sandbox escalation; the project execpolicy will request approval.",
    "Do not retry it inside the sandbox.",
  ].join(" "));
}

function candidateModuleRoots() {
  const roots = [];
  if (process.env.PLAYWRIGHT_NODE_MODULES) {
    roots.push(process.env.PLAYWRIGHT_NODE_MODULES);
  }
  if (process.env.NODE_PATH) {
    roots.push(...process.env.NODE_PATH.split(path.delimiter));
  }
  if (process.env.HOME) {
    roots.push(path.join(process.env.HOME, ".cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules"));
  }
  return [...new Set(roots)].filter((root) => root && fs.existsSync(path.join(root, "playwright")));
}

// Launch the requested browser; when the stock chromium build is missing
// (Playwright installed without `npx playwright install`), fall back to
// the locally installed Chrome via the "chrome" channel.
export async function launchBrowser(browserType, browserName, launchOptions) {
  assertBrowserLaunchAllowed();
  try {
    return {
      browser: await browserType.launch(launchOptions),
      channel: launchOptions.channel || "",
    };
  } catch (err) {
    if (launchOptions.channel || browserName !== "chromium") {
      throw err;
    }
    try {
      return {
        browser: await browserType.launch({ ...launchOptions, channel: "chrome" }),
        channel: "chrome",
      };
    } catch (chromeErr) {
      throw new Error([
        "chromium launch failed, and the fallback to installed Chrome also failed.",
        `chromium: ${String(err?.message || err)}`,
        `chrome: ${String(chromeErr?.message || chromeErr)}`,
      ].join("\n"));
    }
  }
}

export async function createPairingSession(baseURL, publicURL) {
  const res = await fetch(`${baseURL}/api/pairing/sessions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ public_url: publicURL }),
    signal: AbortSignal.timeout(10000),
  });
  if (!res.ok) {
    throw new Error(`create pairing session failed ${res.status}: ${await res.text()}`);
  }
  return res.json();
}
