import { readFile } from "node:fs/promises";

const indexHTML = await readFile(new URL("../index.html", import.meta.url), "utf8");
const productionIDs = new Set(
  [...indexHTML.matchAll(/\bid=(["'])([^"']+)\1/g)].map((match) => match[2]),
);

const words = (value) => String(value).split(/\s+/).filter(Boolean);

class FakeClassList {
  constructor() { this.values = new Set(); }
  setFromString(value) { this.values = new Set(words(value)); }
  add(...values) { values.forEach((value) => this.values.add(String(value))); }
  remove(...values) { values.forEach((value) => this.values.delete(String(value))); }
  toggle(value, force) {
    const key = String(value);
    const enabled = force === undefined ? !this.values.has(key) : Boolean(force);
    if (enabled) this.values.add(key); else this.values.delete(key);
    return enabled;
  }
  contains(value) { return this.values.has(String(value)); }
  [Symbol.iterator]() { return this.values[Symbol.iterator](); }
  toString() { return [...this.values].join(" "); }
}

class FakeElement {
  constructor(tagName = "div") {
    Object.assign(this, {
      attributes: new Map(), children: [], classList: new FakeClassList(), dataset: {},
      disabled: false, hidden: false, listeners: new Map(), open: false, parentElement: null,
      style: {}, tagName: String(tagName).toUpperCase(), title: "", type: "", value: "", _textContent: "",
    });
  }
  get className() { return this.classList.toString(); }
  set className(value) { this.classList.setFromString(value); }
  get textContent() {
    return this._textContent + this.children
      .map((child) => (typeof child === "string" ? child : child?.textContent || "")).join("");
  }
  get childElementCount() { return this.children.filter((child) => child && typeof child === "object").length; }
  set textContent(value) {
    this._textContent = String(value ?? "");
    this.children = [];
  }
  adopt(children) {
    children.forEach((child) => { if (child && typeof child === "object") child.parentElement = this; });
    return children;
  }
  append(...children) { this.children.push(...this.adopt(children)); }
  appendChild(child) {
    this.children.push(...this.adopt([child]));
    return child;
  }
  replaceChildren(...children) {
    this._textContent = "";
    this.children = [...this.adopt(children)];
  }
  addEventListener(type, listener) {
    this.listeners.set(type, [...(this.listeners.get(type) || []), listener]);
  }
  dispatchEvent(event = {}) {
    const value = { ...event, currentTarget: this, target: event.target || this, type: event.type || "" };
    for (const listener of this.listeners.get(value.type) || []) listener.call(this, value);
    return true;
  }
  click() {
    this.dispatchEvent({ type: "click" });
  }
  contains(target) { return target === this || this.children.some((child) => child?.contains?.(target)); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  removeAttribute(name) { this.attributes.delete(name); }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  close() { this.open = false; }
  showModal() { this.open = true; }
  scrollIntoView() {}
}

function createDOMHarness({ visibilityState = "visible", querySelector = () => null, querySelectorAll = () => [] } = {}) {
  const elements = new Map();
  function element(id) {
    if (!productionIDs.has(id)) return null;
    if (!elements.has(id)) elements.set(id, new FakeElement());
    return elements.get(id);
  }
  const document = {
    visibilityState, body: new FakeElement("body"), addEventListener() {},
    createElement: (tagName) => new FakeElement(tagName), createElementNS: (_namespace, tagName) => new FakeElement(tagName),
    createTextNode: (value) => String(value ?? ""),
    getElementById: element, querySelector, querySelectorAll,
  };
  return { document, element, elements };
}

export { FakeElement, createDOMHarness, productionIDs };
