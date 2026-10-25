import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { runInNewContext } from "node:vm";
import { ICONS, buildIconURL, createApp, messageForStatus, validateInput } from "./app.js";

const sizes = [16, 32, 48, 180, 192, 512];
const source = "https://origin.test/a%2fb.png?token=a%2Bb&slash=%2f&padding=x%3D%3D";
const signature = "Ab_09-".repeat(7) + "A=";

// A deliberately small DOM fixture. Events bubble through the form, while
// fetch, response bodies and object URLs remain observable dependencies of the
// real controller. No third-party DOM or browser implementation is needed.
class Element {
  constructor(document, tagName = "div", id = "") {
    this.ownerDocument = document;
    this.tagName = tagName.toUpperCase();
    this.id = id;
    this.value = "";
    this.disabled = false;
    this.hidden = false;
    this.textContent = "";
    this.src = "";
    this.href = "";
    this.download = "";
    this.children = [];
    this.listeners = new Map();
    this.attributes = new Map();
    this.dataset = {};
    const classes = new Set();
    this.classList = {
      add: (...names) => names.forEach((name) => classes.add(name)),
      remove: (...names) => names.forEach((name) => classes.delete(name)),
      contains: (name) => classes.has(name),
      toggle: (name, force) => {
        const on = force ?? !classes.has(name);
        if (on) classes.add(name); else classes.delete(name);
        return on;
      },
    };
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter((item) => item !== listener));
  }

  emit(type) {
    const event = { type, target: this, preventDefault() {}, stopPropagation() { this.stopped = true; } };
    const results = [];
    for (let node = this; node; node = node.parentNode) {
      for (const listener of node.listeners.get(type) ?? []) results.push(listener(event));
      if (event.stopped) break;
    }
    return Promise.all(results);
  }

  click() {
    if (this.disabled) return Promise.resolve();
    if (this.tagName === "A") this.ownerDocument.downloads.push({ href: this.href, download: this.download });
    return this.emit("click");
  }

  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);
    return child;
  }

  remove() {
    if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((child) => child !== this);
    this.parentNode = null;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
    if (name === "src") this.src = String(value);
  }

  getAttribute(name) { return name === "src" ? (this.src || null) : (this.attributes.get(name) ?? null); }
  removeAttribute(name) { this.attributes.delete(name); if (name === "src") this.src = ""; }
  focus() { this.ownerDocument.activeElement = this; }
}

class Document {
  constructor() {
    this.elements = new Map();
    this.downloads = [];
    this.body = new Element(this, "body");
    const form = this.add("icon-form", "form");
    for (const id of ["source-url", "source-signature", "app-name", "app-short-name"]) form.appendChild(this.add(id, "input"));
    for (const id of ["generate-button", "cancel-button", "download-button"]) form.appendChild(this.add(id, "button"));
    this.add("status-message");
    for (const size of sizes) {
      this.add(`image-${size}`, "img").hidden = true;
      this.add(`state-${size}`, "span");
    }
    this.getElementById("cancel-button").disabled = true;
    this.getElementById("download-button").disabled = true;
  }

  add(id, tagName) {
    const node = new Element(this, tagName, id);
    this.elements.set(id, node);
    return node;
  }

  getElementById(id) { return this.elements.get(id) ?? null; }
  querySelector(selector) { return selector.startsWith("#") ? this.getElementById(selector.slice(1)) : null; }
  createElement(tagName) { return new Element(this, tagName); }
}

async function tick() { await new Promise((resolve) => setImmediate(resolve)); }

async function until(predicate, message) {
  for (let i = 0; i < 30; i++) {
    if (predicate()) return;
    await tick();
  }
  assert.ok(predicate(), message);
}

function fixture(mount = true) {
  const document = new Document();
  document.getElementById("source-url").value = source;
  document.getElementById("source-signature").value = signature;
  document.getElementById("app-name").value = " 测试应用 🙂 ";
  document.getElementById("app-short-name").value = " 图标 ";
  const calls = [];
  let active = 0;
  let maxActive = 0;
  const fetch = (url, init) => {
    const call = { url: String(url), init, settled: false, bodyReads: 0 };
    active++;
    maxActive = Math.max(maxActive, active);
    calls.push(call);
    return new Promise((resolve, reject) => {
      call.resolve = (status = 200, payload = call.url, type = "image/png") => {
        assert.equal(call.settled, false, "fetch resolved twice");
        call.settled = true;
        active--;
        resolve({
          ok: status >= 200 && status < 300,
          status,
          blob: async () => { call.bodyReads++; return new Blob([payload], { type }); },
          text: async () => { call.bodyReads++; return "SECRET upstream response https://private.test/token"; },
          json: async () => { call.bodyReads++; return { message: "SECRET upstream response" }; },
        });
      };
      call.reject = (error) => {
        assert.equal(call.settled, false, "fetch rejected twice");
        call.settled = true;
        active--;
        reject(error);
      };
    });
  };
  const created = new Map();
  const revoked = [];
  const timers = [];
  const URL = {
    createObjectURL(blob) {
      const value = `blob:fixture/${created.size + 1}`;
      created.set(value, blob);
      return value;
    },
    revokeObjectURL(value) { revoked.push(value); },
  };
  const dependencies = { document, fetch, URL, AbortController, setTimeout: (fn) => { timers.push(fn); return timers.length; } };
  const app = mount ? createApp(dependencies) : null;
  return {
    app, dependencies, document, calls, created, revoked, timers,
    get maxActive() { return maxActive; },
    node: (id) => document.getElementById(id),
    submit: () => document.getElementById("icon-form").emit("submit"),
    async change(id, value) { const node = document.getElementById(id); node.value = value; await node.emit("input"); },
  };
}

async function finishPreview(f, start = 0, marker = "current") {
  for (let i = 0; i < sizes.length; i++) {
    await until(() => f.calls.length === start + i + 1, `preview ${i + 1} did not start`);
    const call = f.calls[start + i];
    assert.match(call.url, /^\/icons\/[^?]+\.png\?/);
    await tick();
    assert.equal(f.calls.length, start + i + 1, "previews must be fetched serially");
    call.resolve(200, `${marker}:${i}`);
  }
  await until(() => !f.node("download-button").disabled, "completed previews did not enable ZIP");
}

test("query construction preserves the original URL and HMAC bytes", () => {
  assert.deepEqual(ICONS.map((icon) => icon.size).sort((a, b) => a - b), sizes);
  const input = { url: source, signature, name: " 网站 🙂 ", shortName: " 图标 " };
  const config = validateInput(input);
  assert.equal(config.url, source);
  assert.equal(config.signature, signature);
  const target = new URL(buildIconURL("icons.zip", config, true), "https://generator.test");
  assert.equal(target.origin, "https://generator.test");
  assert.equal(target.pathname, "/icons/icons.zip");
  assert.equal(target.searchParams.get("url"), source);
  assert.equal(target.searchParams.get("s"), signature);
  assert.equal(target.searchParams.get("name"), input.name);
  assert.equal(target.searchParams.get("shortName"), input.shortName);
  const png = new URL(buildIconURL("favicon-32x32.png", config), target);
  assert.equal(png.searchParams.has("name"), false);
  assert.equal(png.searchParams.has("shortName"), false);
});

test("input validation rejects unsafe syntax and uses Unicode character limits", () => {
  assert.equal(validateInput({ url: ` \t${source}\n` }).url, source);
  for (const url of ["", "/relative.png", "ftp://origin.test/a", "javascript:alert(1)", "https://user:password@origin.test/a", "https://origin.test/a#png", "https://origin.test/a#", "https://origin.test/a b", "https://origin.test\\a"]) {
    assert.throws(() => validateInput({ url }), (error) => {
      assert.match(error.message, /[\u4e00-\u9fff]/);
      return true;
    }, url);
  }
  const defaults = validateInput({ url: source });
  assert.equal(defaults.name, "App");
  assert.equal(defaults.shortName, "App");
  const empty = validateInput({ url: source, name: "", shortName: "" });
  assert.equal(empty.name, "App");
  assert.equal(empty.shortName, "App");
  assert.equal(validateInput({ url: source, name: "🙂".repeat(128), shortName: "界".repeat(32) }).name, "🙂".repeat(128));
  for (const fields of [{ name: "🙂".repeat(129) }, { shortName: "界".repeat(33) }, { name: "   " }, { name: "app\u0000" }, { name: "app\u0085" }, { name: "\ud800" }]) {
    assert.throws(() => validateInput({ url: source, ...fields }), /[\u4e00-\u9fff]/);
  }
  const prefix = "https://origin.test/";
  assert.equal(validateInput({ url: prefix + "a".repeat(4096 - prefix.length) }).url.length, 4096);
  assert.throws(() => validateInput({ url: prefix + "a".repeat(4097 - prefix.length) }), /[\u4e00-\u9fff]/);
  assert.throws(() => validateInput({ url: prefix + "界".repeat(1366) }), /[\u4e00-\u9fff]/);
  for (const signature of ["x".repeat(129), "secret\nkey", "\ud800"]) {
    assert.throws(() => validateInput({ url: source, signature }), /[\u4e00-\u9fff]/);
  }
});

test("invalid form input never starts requests and focuses the offending field", async (t) => {
  for (const [id, value] of [["source-url", "javascript:SECRET"], ["app-name", "   "], ["app-short-name", "🙂".repeat(33)]]) {
    const f = fixture();
    t.after(() => f.app.destroy());
    f.node(id).value = value;
    await f.submit();
    await tick();
    assert.equal(f.calls.length, 0);
    assert.equal(f.document.activeElement, f.node(id));
    assert.equal(f.node("download-button").disabled, true);
    assert.match(f.node("status-message").textContent, /[\u4e00-\u9fff]/);
    assert.equal(f.node("status-message").textContent.includes("SECRET"), false);
  }
});

test("the real form controller fetches six previews serially through the local API", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  const submitted = f.submit();
  await finishPreview(f);
  await submitted;
  assert.equal(f.maxActive, 1);
  assert.equal(f.calls.length, 6);
  for (const call of f.calls) {
    const target = new URL(call.url, "https://generator.test");
    assert.equal(target.origin, "https://generator.test");
    assert.equal(target.searchParams.get("url"), source);
    assert.equal(target.searchParams.get("s"), signature);
    assert.equal(call.init.cache, "no-store");
    assert.equal(call.init.referrerPolicy, "no-referrer");
    assert.equal(call.init.credentials, "same-origin");
    assert.equal(call.init.mode, "same-origin");
    assert.equal(call.init.redirect, "error");
    assert.ok(call.init.signal instanceof AbortSignal);
  }
  for (const size of sizes) {
    assert.match(f.node(`image-${size}`).src, /^blob:fixture\//);
    assert.equal(f.node(`image-${size}`).hidden, false);
  }
  assert.equal(f.document.downloads.length, 0);
  assert.equal(f.calls.some((call) => call.url.includes("icons.zip")), false);
});

test("ZIP needs a click and uses the successfully previewed source and metadata snapshot", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  await f.app.download();
  assert.equal(f.calls.length, 0, "ZIP must not fetch before a successful preview");
  void f.submit();
  await finishPreview(f);
  // Programmatic changes without an input event cannot silently replace the
  // configuration whose previews were shown to the user.
  f.node("source-url").value = "https://different.test/not-previewed";
  f.node("source-signature").value = "different";
  f.node("app-name").value = "not previewed";
  f.node("app-short-name").value = "other";
  const download = f.node("download-button").click();
  await until(() => f.calls.length === 7, "ZIP click did not start a request");
  const target = new URL(f.calls[6].url, "https://generator.test");
  assert.equal(target.pathname, "/icons/icons.zip");
  assert.equal(target.searchParams.get("url"), source);
  assert.equal(target.searchParams.get("s"), signature);
  assert.equal(target.searchParams.get("name"), " 测试应用 🙂 ");
  assert.equal(target.searchParams.get("shortName"), " 图标 ");
  assert.equal(f.calls[6].init.mode, "same-origin");
  assert.equal(f.calls[6].init.redirect, "error");
  assert.equal(f.calls[6].init.referrerPolicy, "no-referrer");
  f.calls[6].resolve(200, "ZIP fixture", "application/zip");
  await download;
  await until(() => f.document.downloads.length === 1, "ZIP was not offered as a download");
  const anchor = f.document.downloads[0];
  assert.equal(anchor.download, "icons.zip");
  assert.match(anchor.href, /^blob:fixture\//);
  assert.equal(f.document.body.children.length, 0, "temporary download link was retained");
  assert.ok(f.timers.length > 0, "ZIP object URL has no deferred cleanup");
  for (const callback of f.timers) callback();
  assert.ok(f.revoked.includes(anchor.href), "ZIP object URL was not released");
});

test("API failures produce fixed Chinese messages without reading upstream bodies", async (t) => {
  for (const status of [400, 401, 403, 404, 413, 429, 500, 502, 503, 504]) {
    const f = fixture();
    t.after(() => f.app.destroy());
    void f.submit();
    await until(() => f.calls.length === 1, "preview did not start");
    f.calls[0].resolve(status, "SECRET response", "text/plain");
    await until(() => !f.node("generate-button").disabled, "API failure did not settle");
    const message = f.node("status-message").textContent;
    assert.match(message, /[\u4e00-\u9fff]/);
    assert.equal(message.includes("SECRET"), false);
    assert.equal(message.includes("origin.test"), false);
    assert.equal(message.includes("a%2Bb"), false);
    assert.equal(f.calls[0].bodyReads, 0);
    assert.equal(f.calls.length, 1, "failure continued fetching previews");
    assert.equal(f.node("download-button").disabled, true);
    assert.match(messageForStatus(status), /[\u4e00-\u9fff]/);
  }
});

test("changing input aborts stale requests and late responses cannot replace current previews", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  void f.submit();
  await until(() => f.calls.length === 1, "old preview did not start");
  await f.change("source-url", "https://new.test/logo.png?token=NEW");
  assert.equal(f.calls[0].init.signal.aborted, true);
  assert.equal(f.node("download-button").disabled, true);
  void f.submit();
  await until(() => f.calls.length === 2, "replacement preview did not start");
  f.calls[0].resolve(200, "STALE response");
  await tick();
  assert.equal(f.calls[0].bodyReads, 0, "cancelled response body was still consumed");
  assert.equal(f.calls.length, 2, "stale preview started another fetch");
  await finishPreview(f, 1, "NEW response");
  for (const size of sizes) {
    const image = f.node(`image-${size}`);
    assert.equal(image.hidden, false);
    assert.match(await f.created.get(image.src).text(), /^NEW response:/);
  }
  const current = sizes.map((size) => f.node(`image-${size}`).src);
  await f.change("app-name", "different metadata");
  assert.equal(f.node("download-button").disabled, true);
  for (const value of current) assert.ok(f.revoked.includes(value), "input change retained a preview blob");
  for (const size of sizes) {
    assert.equal(f.node(`image-${size}`).hidden, true);
    assert.equal(f.node(`image-${size}`).src, "");
  }
  await f.app.download();
  assert.equal(f.calls.length, 7, "edited metadata allowed ZIP from an obsolete preview");
});

test("an old image decode cannot restart work after a changed input", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  let finishDecode;
  f.node("image-16").decode = () => new Promise((resolve) => { finishDecode = resolve; });
  void f.submit();
  await until(() => f.calls.length === 1, "preview did not start");
  f.calls[0].resolve(200, "old decoded image");
  await until(() => !!finishDecode, "image decode was not awaited");
  const oldURL = f.node("image-16").src;
  await f.change("source-url", "https://new.test/logo");
  assert.ok(f.revoked.includes(oldURL));
  delete f.node("image-16").decode;
  finishDecode();
  await tick();
  assert.equal(f.calls.length, 1, "late decode restarted a stale preview sequence");
  assert.equal(f.node("image-16").src, "");
  assert.equal(f.node("download-button").disabled, true);
  assert.notEqual(f.node("state-16").textContent, "已就绪");
});

test("a cancelled ZIP response cannot start a download or alter replacement previews", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  void f.submit();
  await finishPreview(f);
  void f.node("download-button").click();
  await until(() => f.calls.length === 7, "ZIP did not start");
  await f.change("source-url", "https://new.test/other-logo");
  assert.equal(f.calls[6].init.signal.aborted, true);
  void f.submit();
  await finishPreview(f, 7, "replacement");
  const currentURLs = sizes.map((size) => f.node(`image-${size}`).src);
  const currentStatus = f.node("status-message").textContent;
  f.calls[6].resolve(200, "STALE ZIP", "application/zip");
  await tick();
  assert.equal(f.calls[6].bodyReads, 0, "cancelled ZIP body was still consumed");
  assert.equal(f.document.downloads.length, 0);
  assert.equal(f.node("status-message").textContent, currentStatus);
  assert.equal(f.node("download-button").disabled, false);
  assert.deepEqual(sizes.map((size) => f.node(`image-${size}`).src), currentURLs);
});

test("cancel and destroy abort work and release every owned preview object URL", async (t) => {
  const f = fixture();
  t.after(() => f.app.destroy());
  void f.submit();
  await until(() => f.calls.length === 1, "preview did not start");
  f.calls[0].resolve(200, "first preview");
  await until(() => f.calls.length === 2, "second preview did not start");
  const first = f.node("image-16").src;
  assert.match(first, /^blob:/);
  await f.node("cancel-button").click();
  assert.equal(f.calls[1].init.signal.aborted, true);
  assert.ok(f.revoked.includes(first));
  assert.equal(f.node("download-button").disabled, true);
  f.calls[1].resolve(200, "late cancelled response");
  await tick();
  assert.equal(f.calls.length, 2);
  assert.equal(f.node("image-32").src, "");
  void f.submit();
  await finishPreview(f, 2);
  const owned = [...f.created.keys()];
  f.app.destroy();
  for (const value of owned) assert.ok(f.revoked.includes(value), `destroy retained ${value}`);
});

test("network errors and invalid blobs never expose remote error text or enable ZIP", async (t) => {
  for (const failure of ["network", "type", "empty"]) {
    const f = fixture();
    t.after(() => f.app.destroy());
    void f.submit();
    await until(() => f.calls.length === 1, "preview did not start");
    if (failure === "network") f.calls[0].reject(new Error("SECRET https://private.test/token"));
    if (failure === "type") f.calls[0].resolve(200, "not PNG", "text/html");
    if (failure === "empty") f.calls[0].resolve(200, "", "image/png");
    await until(() => !f.node("generate-button").disabled, "invalid response did not settle");
    assert.equal(f.node("download-button").disabled, true);
    assert.match(f.node("status-message").textContent, /[\u4e00-\u9fff]/);
    assert.doesNotMatch(f.node("status-message").textContent, /SECRET|private\.test|token/);
    assert.equal(f.calls.length, 1);
  }
});

test("browser pagehide cancels and releases previews while BFCache restoration remains usable", async (t) => {
  const f = fixture(false);
  const listeners = new Map();
  class BrowserURL extends URL {
    static createObjectURL(blob) { return f.dependencies.URL.createObjectURL(blob); }
    static revokeObjectURL(value) { f.dependencies.URL.revokeObjectURL(value); }
  }
  const context = {
    document: f.document,
    fetch: f.dependencies.fetch,
    URL: BrowserURL,
    URLSearchParams, AbortController, TextEncoder,
    setTimeout: f.dependencies.setTimeout,
    addEventListener(type, callback) { listeners.set(type, callback); },
  };
  // Run the real browser entry point in an isolated global object. Removing
  // module export keywords is the only adaptation; no controller is replaced.
  const script = readFileSync(new URL("./app.js", import.meta.url), "utf8").replace(/^export /gm, "");
  runInNewContext(script, context, { filename: "app.js" });
  assert.equal(typeof listeners.get("pagehide"), "function");
  const hide = () => listeners.get("pagehide")({ persisted: true });
  t.after(hide);
  void f.submit();
  await until(() => f.calls.length === 1, "browser bootstrap did not bind the form");
  f.calls[0].resolve(200, "before pagehide");
  await until(() => f.calls.length === 2, "browser preview did not advance");
  const first = f.node("image-16").src;
  hide();
  assert.equal(f.calls[1].init.signal.aborted, true);
  assert.ok(f.revoked.includes(first));
  assert.equal(f.node("download-button").disabled, true);
  f.calls[1].resolve(200, "late hidden page response");
  await tick();
  assert.equal(f.node("image-32").src, "");
  // A persisted page restores its existing listeners instead of booting again.
  void f.submit();
  await finishPreview(f, 2, "after BFCache restoration");
  hide();
  for (const value of f.created.keys()) assert.ok(f.revoked.includes(value));
});
