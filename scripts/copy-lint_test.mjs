import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const lint = fileURLToPath(new URL("./copy-lint.mjs", import.meta.url));

function check(t, html) {
  const dir = mkdtempSync(join(tmpdir(), "canary-copy-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "page.html");
  writeFileSync(file, html);
  return spawnSync(process.execPath, [lint, file], { encoding: "utf8" });
}

test("copy checks ignore script text with browser-accepted closing tags", (t) => {
  for (const closing of ["</script >", "</SCRIPT ignored>", "</script/>"]) {
    const result = check(t, `<script>const hidden = "this is coming soon";${closing}<p>Visible words remain here.</p>`);
    assert.equal(result.status, 0, result.stdout + result.stderr);
  }
});

test("copy checks decode each entity only once", (t) => {
  for (const encoded of ["&amp;lt;", "&#38;lt;", "&#x26;lt;"]) {
    const result = check(t, `<p>The robust example contains ${encoded} today.</p>`);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.ok(result.stdout.includes("contains &lt; today"), result.stdout);
  }
});

test("script-like custom tags do not hide visible copy", (t) => {
  const result = check(t, "<script-note>This is coming soon.</script-note>");
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.ok(result.stdout.includes("coming soon"), result.stdout);
});

test("unknown named entities remain literal", (t) => {
  const result = check(t, "<p>The robust example contains &constructor; today.</p>");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.ok(result.stdout.includes("contains &constructor; today"), result.stdout);
});

test("script closing prefixes do not expose script text as prose", (t) => {
  const result = check(t, '<script>const example = "</scripture>This is coming soon.";</script><p>Visible words remain here.</p>');
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test("invalid numeric entities do not crash the copy check", (t) => {
  const result = check(t, "<p>The robust example contains &#1114112; today.</p>");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.ok(result.stdout.includes("contains \ufffd today"), result.stdout);
});
