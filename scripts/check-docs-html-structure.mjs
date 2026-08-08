// Structural gate for every docs/ HTML page, hand-authored ones included.
// The generated pages have a byte-exact parity check; hand-authored pages had
// nothing, and a missing brace in an inline <style> block ships invisibly —
// CSS error recovery silently drops the swallowed rules (the .callout block in
// claude-desktop-interactive-brokers/ was unstyled in production this way).
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const root = process.argv[2] ?? "docs";
const failures = [];

function walk(dir) {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      walk(path);
    } else if (entry.endsWith(".html")) {
      checkFile(path);
    }
  }
}

// Brace balance per <style> block, with comments and quoted strings stripped
// so a brace inside content:"{" or /* { */ cannot skew the count.
function checkFile(path) {
  const html = readFileSync(path, "utf8");
  const styleBlocks = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)];
  const openStyles = (html.match(/<style[^>]*>/gi) ?? []).length;
  if (openStyles !== styleBlocks.length) {
    failures.push(`${path}: unclosed <style> tag`);
    return;
  }
  for (const block of styleBlocks) {
    const css = block[1]
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/"(?:[^"\\]|\\.)*"/g, '""')
      .replace(/'(?:[^'\\]|\\.)*'/g, "''");
    let depth = 0;
    let minDepth = 0;
    for (const ch of css) {
      if (ch === "{") depth++;
      else if (ch === "}") depth--;
      if (depth < minDepth) minDepth = depth;
    }
    if (depth !== 0 || minDepth < 0) {
      const line = html.slice(0, block.index).split("\n").length;
      failures.push(
        `${path}:${line}: <style> block braces unbalanced (open-close delta ${depth}${minDepth < 0 ? ", stray close" : ""})`,
      );
    }
  }
}

walk(root);
if (failures.length > 0) {
  console.error("docs-html-structure: FAIL");
  for (const failure of failures) console.error("  " + failure);
  process.exit(1);
}
console.log("docs-html-structure: OK");
