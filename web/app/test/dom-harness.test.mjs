import assert from "node:assert/strict";
import test from "node:test";

import { FakeElement, createDOMHarness, productionIDs } from "./dom-harness.mjs";

test("DOM harness exposes only ids present in production index.html", () => {
  const { document } = createDOMHarness();
  assert.ok(productionIDs.has("dashboard"), "production fixture should include dashboard");
  assert.ok(document.getElementById("dashboard"));
  assert.equal(document.getElementById("governanceCurrentStateTypo"), null);
});

test("DOM harness mirrors text, children, and class mutations used by renderers", () => {
  const element = new FakeElement();
  const child = new FakeElement();
  child.textContent = "child";
  element.append(child);
  assert.equal(element.textContent, "child");

  element.textContent = "replacement";
  assert.equal(element.children.length, 0);
  assert.equal(element.textContent, "replacement");

  element.replaceChildren(child);
  assert.equal(element.textContent, "child");
  element.replaceChildren();
  assert.equal(element.textContent, "");

  element.className = "alpha beta";
  assert.equal(element.classList.contains("alpha"), true);
  element.classList.remove("alpha");
  element.classList.add("gamma");
  assert.equal(element.className, "beta gamma");
});
