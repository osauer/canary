import { readdir, readFile, stat } from "node:fs/promises";
import path from "node:path";

const projectBase = "/canary";
const siteOrigin = "https://osauer.dev";
const root = path.join(process.cwd(), "docs");
const problems = [];

async function exists(file) {
  try {
    await stat(file);
    return true;
  } catch (err) {
    if (err && err.code === "ENOENT") {
      return false;
    }
    throw err;
  }
}

function localPath(raw) {
  if (!raw || raw.startsWith("#")) {
    return "";
  }
  if (/^(mailto|tel|javascript):/i.test(raw)) {
    return "";
  }
  let url;
  if (raw.startsWith(`${projectBase}/`) || raw === projectBase) {
    url = new URL(raw, siteOrigin);
  } else {
    try {
      url = new URL(raw);
    } catch {
      return "";
    }
  }
  if (url.origin !== siteOrigin) {
    return "";
  }
  if (url.pathname !== projectBase && !url.pathname.startsWith(`${projectBase}/`)) {
    return "";
  }
  return decodeURIComponent(url.pathname.slice(projectBase.length)) || "/";
}

function targetFile(pathname) {
  if (!pathname || pathname === "/") {
    return "index.html";
  }
  const clean = pathname.replace(/^\/+/, "");
  if (pathname.endsWith("/")) {
    return path.join(clean, "index.html");
  }
  return clean;
}

async function checkTarget(source, raw) {
  const target = localPath(raw);
  if (!target) {
    return;
  }
  const file = targetFile(target);
  if (!(await exists(path.join(root, file)))) {
    problems.push(`${source}: ${raw} -> missing docs/${file}`);
  }
}

function referencedURLs(data) {
  const out = [];
  for (const match of data.matchAll(/\b(?:href|src)=["']([^"']+)["']/gi)) {
    out.push(match[1]);
  }
  for (const match of data.matchAll(/<loc>\s*([^<]+?)\s*<\/loc>/gi)) {
    out.push(match[1]);
  }
  for (const match of data.matchAll(/https:\/\/osauer\.dev\/canary\/[^\s"'<>)&]+/g)) {
    out.push(match[0].replace(/[.,;:]+$/, ""));
  }
  return out;
}

async function* walk(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walk(file);
      continue;
    }
    yield file;
  }
}

for await (const file of walk(root)) {
  if (!/\.(html|xml|json|txt)$/.test(file)) {
    continue;
  }
  const data = await readFile(file, "utf8");
  if (/https:\/\/osauer\.dev\/ibkr(?:\/|["'<\s])/i.test(data) ||
      /\b(?:href|src)=["']\/ibkr(?:\/|["'])/i.test(data)) {
    problems.push(`${path.relative(process.cwd(), file)}: contains retired /ibkr public base`);
  }
  for (const raw of referencedURLs(data)) {
    await checkTarget(path.relative(process.cwd(), file), raw);
  }
}

if (problems.length > 0) {
  console.error(`pages link check failed with ${problems.length} problem(s):`);
  for (const problem of problems) {
    console.error(`  - ${problem}`);
  }
  process.exit(1);
}

console.log("pages link check: all osauer.dev/canary links resolve under docs/");
