import { readFile } from "node:fs/promises";

const indexHTML = await readFile(new URL("../index.html", import.meta.url), "utf8");
const productionIDs = new Set(
  [...indexHTML.matchAll(/\bid=(["'])([^"']+)\1/g)].map((match) => match[2]),
);

class FakeClassList {
  constructor(owner) {
    this.owner = owner;
    this.values = new Set();
  }
  setFromString(value) {
    this.values = new Set(String(value).split(/\s+/).filter(Boolean));
  }
  add(...values) {
    values.forEach((value) => this.values.add(String(value)));
  }
  remove(...values) {
    values.forEach((value) => this.values.delete(String(value)));
  }
  toggle(value, force) {
    const key = String(value);
    const enabled = force === undefined ? !this.values.has(key) : Boolean(force);
    if (enabled) this.values.add(key); else this.values.delete(key);
    return enabled;
  }
  contains(value) {
    return this.values.has(String(value));
  }
  [Symbol.iterator]() {
    return this.values[Symbol.iterator]();
  }
  toString() {
    return [...this.values].join(" ");
  }
}

class FakeElement {
  constructor() {
    this.attributes = new Map();
    this.children = [];
    this.classList = new FakeClassList(this);
    this.dataset = {};
    this.disabled = false;
    this.hidden = false;
    this.open = false;
    this.title = "";
    this._textContent = "";
  }
  get className() {
    return this.classList.toString();
  }
  set className(value) {
    this.classList.setFromString(value);
  }
  get textContent() {
    const childText = this.children.map((child) => (
      typeof child === "string" ? child : child?.textContent || ""
    )).join("");
    return this._textContent + childText;
  }
  set textContent(value) {
    this._textContent = String(value ?? "");
    this.children = [];
  }
  append(...children) {
    this.children.push(...children);
  }
  appendChild(child) {
    this.children.push(child);
    return child;
  }
  replaceChildren(...children) {
    this._textContent = "";
    this.children = [...children];
  }
  addEventListener() {}
  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
  scrollIntoView() {}
}

function createDOMHarness({ visibilityState = "visible", querySelectorAll = () => [] } = {}) {
  const elements = new Map();
  function element(id) {
    if (!productionIDs.has(id)) return null;
    if (!elements.has(id)) elements.set(id, new FakeElement());
    return elements.get(id);
  }
  const document = {
    visibilityState,
    addEventListener() {},
    createElement: () => new FakeElement(),
    createElementNS: () => new FakeElement(),
    getElementById: element,
    querySelectorAll,
  };
  return { document, element, elements };
}

export { FakeElement, createDOMHarness, productionIDs };
