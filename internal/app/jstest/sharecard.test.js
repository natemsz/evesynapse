// Tests for the share card <dialog> controller in ../static/app.js:
// native dialog open/close, eager image pre-fetching, PNG normalization
// for fallback JPEG cards, Android-safe Copy image (ready Blob + Promise
// fallback + failure feedback), desktop/mobile Share fallback to link+title,
// and retargeting/retry behavior.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");

function makeStubEnv() {
  const docListeners = {};
  const winListeners = {};
  const elementsById = {};

  function hasClass(el, cls) {
    return (el.className || "").split(/\s+/).indexOf(cls) >= 0;
  }

  class El {
    constructor(tag) {
      this.tagName = (tag || "div").toUpperCase();
      this.className = "";
      this.id = "";
      this.attrs = {};
      this.children = [];
      this.parentNode = null;
      this.hidden = false;
      this.open = false;
      this.textContent = "";
      this.value = "";
      this.style = {};
      this.focused = false;
      this.complete = false;
      this.naturalWidth = 0;
      this.naturalHeight = 0;
      this._rect = { left: 100, right: 500, top: 100, bottom: 500 };
      const self = this;
      this.classList = {
        add(k) { if (!hasClass(self, k)) self.className = (self.className + " " + k).trim(); },
        remove(k) { self.className = self.className.split(/\s+/).filter((x) => x && x !== k).join(" "); },
        contains(k) { return hasClass(self, k); },
      };
    }
    setAttribute(k, v) {
      this.attrs[k] = String(v);
      if (k === "id") {
        this.id = String(v);
        elementsById[this.id] = this;
      }
      if (k === "src") {
        this.src = String(v);
      }
    }
    getAttribute(k) {
      if (k === "src") return this.attrs.src !== undefined ? this.attrs.src : (this.src || null);
      return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
    }
    removeAttribute(k) {
      delete this.attrs[k];
      if (k === "src") {
        this.src = "";
        this.currentSrc = "";
      }
    }
    hasAttribute(k) {
      return Object.prototype.hasOwnProperty.call(this.attrs, k);
    }
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    }
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx >= 0) this.children.splice(idx, 1);
      child.parentNode = null;
      return child;
    }
    focus() { this.focused = true; }
    select() {}
    getBoundingClientRect() { return this._rect; }
    matches(sel) {
      if (sel === "[data-share-card]") return this.hasAttribute("data-share-card");
      if (sel === ".share-modal") return this.classList.contains("share-modal");
      if (sel === "[data-close-modal]") return this.hasAttribute("data-close-modal");
      if (sel === "[data-share-copy-link]") return this.hasAttribute("data-share-copy-link");
      if (sel === "[data-share-copy-image]") return this.hasAttribute("data-share-copy-image");
      if (sel === "[data-share-native]") return this.hasAttribute("data-share-native");
      if (sel === "dialog.share-modal[open]") return this.tagName === "DIALOG" && this.classList.contains("share-modal") && this.open;
      return false;
    }
    closest(sel) {
      let cur = this;
      while (cur) {
        if (cur.matches(sel)) return cur;
        cur = cur.parentNode;
      }
      return null;
    }
    querySelector(sel) {
      return this.querySelectorAll(sel)[0] || null;
    }
    querySelectorAll(sel) {
      const out = [];
      const selectors = sel.split(",").map((s) => s.trim());
      const visit = (node) => {
        for (const c of node.children) {
          for (const s of selectors) {
            if (
              (s === "img[data-share-src]" && c.tagName === "IMG" && c.hasAttribute("data-share-src")) ||
              (s === ".share-wait" && c.classList.contains("share-wait")) ||
              (s === "[data-share-copy-link]" && c.hasAttribute("data-share-copy-link")) ||
              (s === "[data-share-copy-image]" && c.hasAttribute("data-share-copy-image")) ||
              (s === "[data-share-native]" && c.hasAttribute("data-share-native")) ||
              (s === "a[download]" && c.tagName === "A" && c.hasAttribute("download")) ||
              (s === ".modal-head h2" && c.tagName === "H2") ||
              (s === "button:not([data-close-modal])" && c.tagName === "BUTTON" && !c.hasAttribute("data-close-modal")) ||
              c.matches(s)
            ) {
              out.push(c);
              break;
            }
          }
          visit(c);
        }
      };
      visit(this);
      return out;
    }
  }

  class DialogEl extends El {
    constructor() {
      super("dialog");
      this.showModalCalls = 0;
      this.closeCalls = 0;
    }
    showModal() {
      this.open = true;
      this.showModalCalls++;
    }
    close() {
      this.open = false;
      this.closeCalls++;
    }
  }

  const root = new El("body");
  let fetchHandler = async () => ({ ok: true, blob: async () => new Blob([new Uint8Array([1, 2, 3])], { type: "image/png" }) });

  global.window = {
    location: { href: "https://evesynapse.app/character/?character=42" },
    isSecureContext: true,
    matchMedia: () => ({ matches: false, addEventListener() {} }),
    addEventListener(ev, fn) { (winListeners[ev] = winListeners[ev] || []).push(fn); },
    setTimeout: (...a) => global.setTimeout(...a),
    clearTimeout: (...a) => global.clearTimeout(...a),
  };
  global.location = global.window.location;
  Object.defineProperty(global, "navigator", {
    value: {
      clipboard: {
        write: async () => {},
        writeText: async () => {},
      },
      share: async () => {},
      canShare: () => true,
    },
    configurable: true,
    writable: true,
  });
  let imageLoadMode = "ok";
  let lastObjectUrl = "";
  const revokedUrls = [];
  const NativeURL = global.URL.NativeURL || global.URL;
  class StubURL extends NativeURL {
    static createObjectURL() {
      lastObjectUrl = "blob:stub-" + Math.random();
      return lastObjectUrl;
    }
    static revokeObjectURL(u) {
      revokedUrls.push(u);
    }
  }
  StubURL.NativeURL = NativeURL;
  global.URL = StubURL;
  global.Image = class StubImage {
    set src(val) {
      this._src = val;
      queueMicrotask(() => {
        this.complete = true;
        if (imageLoadMode === "fail") {
          this.naturalWidth = 0;
          if (this.onerror) this.onerror(new Error("decode failed"));
        } else {
          this.naturalWidth = 400;
          this.naturalHeight = 200;
          if (this.onload) this.onload();
        }
      });
    }
    get src() { return this._src || ""; }
  };
  const origCreate = (tag) => {
    const el = new El(tag);
    if (String(tag).toLowerCase() === "canvas") {
      el.width = 0;
      el.height = 0;
      el.getContext = () => ({ drawImage() {} });
      el.toBlob = (cb, mime) => cb(new Blob([new Uint8Array([9, 8, 7])], { type: mime || "image/png" }));
    }
    return el;
  };
  global.ClipboardItem = class ClipboardItem {
    constructor(data) { this.data = data; }
    static supports(mime) { return mime === "image/png"; }
  };
  global.File = class File {
    constructor(parts, name, opts) {
      this.parts = parts;
      this.name = name;
      this.type = opts && opts.type;
    }
  };
  global.fetch = (url, opts) => fetchHandler(url, opts);
  global.document = {
    documentElement: new El("html"),
    body: root,
    hidden: false,
    addEventListener(ev, fn) { (docListeners[ev] = docListeners[ev] || []).push(fn); },
    getElementById(id) { return elementsById[id] || null; },
    querySelectorAll(sel) { return root.querySelectorAll(sel); },
    querySelector(sel) { return root.querySelector(sel); },
    createElement(tag) { return origCreate(tag); },
    execCommand() { return false; },
  };

  function makeDialog({ id = "pilot-share", image = "/pilot/share.png?character=42", link = "/pilot/?character=42", fileName = "Pilot - EveSynapse.png" } = {}) {
    const dlg = new DialogEl();
    dlg.setAttribute("id", id);
    dlg.classList.add("modal");
    dlg.classList.add("share-card");
    dlg.classList.add("share-modal");

    const head = new El("div");
    head.classList.add("modal-head");
    const h2 = new El("h2");
    h2.textContent = "Share pilot card";
    const closeBtn = new El("button");
    closeBtn.setAttribute("data-close-modal", "");
    head.appendChild(h2);
    head.appendChild(closeBtn);
    dlg.appendChild(head);

    const fig = new El("figure");
    fig.classList.add("share-preview");
    const img = new El("img");
    img.setAttribute("data-share-src", image);
    const wait = new El("figcaption");
    wait.classList.add("share-wait");
    wait.textContent = "Drawing the card…";
    fig.appendChild(img);
    fig.appendChild(wait);
    dlg.appendChild(fig);

    const actions = new El("div");
    actions.classList.add("share-actions");
    const linkBtn = new El("button");
    linkBtn.setAttribute("data-share-copy-link", link);
    linkBtn.textContent = "Copy link";
    const imgBtn = new El("button");
    imgBtn.setAttribute("data-share-copy-image", "");
    imgBtn.hidden = true;
    imgBtn.textContent = "Copy image";
    const dlLink = new El("a");
    dlLink.setAttribute("href", image + "&download=1");
    dlLink.setAttribute("download", fileName);
    const nativeBtn = new El("button");
    nativeBtn.setAttribute("data-share-native", "");
    nativeBtn.hidden = true;
    nativeBtn.textContent = "Share…";
    actions.appendChild(linkBtn);
    actions.appendChild(imgBtn);
    actions.appendChild(dlLink);
    actions.appendChild(nativeBtn);
    dlg.appendChild(actions);

    root.appendChild(dlg);
    return { dlg, img, wait, linkBtn, imgBtn, dlLink, nativeBtn, closeBtn };
  }

  function dispatch(evName, ev) {
    for (const fn of docListeners[evName] || []) fn(ev);
  }

  delete require.cache[require.resolve("../static/app.js")];
  const app = require("../static/app.js");

  return {
    api: app.shareCardTestAPI,
    El,
    makeDialog,
    dispatch,
    root,
    revokedUrls,
    setFetch(fn) { fetchHandler = fn; },
    setImageLoadMode(mode) { imageLoadMode = mode; },
  };
}

test("share card opens native <dialog> via showModal and eagerly fetches image bytes", async () => {
  const env = makeStubEnv();
  const { dlg, img, wait, imgBtn, nativeBtn, linkBtn } = env.makeDialog();

  let fetchCalls = 0;
  env.setFetch(async (url) => {
    fetchCalls++;
    assert.equal(url, "https://evesynapse.app/pilot/share.png?character=42");
    return { ok: true, blob: async () => new Blob([new Uint8Array([9, 8])], { type: "image/png" }) };
  });

  const opener = new env.El("button");
  opener.setAttribute("data-share-card", "pilot-share");
  env.root.appendChild(opener);

  let prevented = false;
  env.dispatch("click", { target: opener, preventDefault() { prevented = true; } });

  assert.equal(prevented, true);
  assert.equal(dlg.showModalCalls, 1);
  assert.equal(dlg.open, true);
  assert.equal(linkBtn.focused, true);
  assert.equal(imgBtn.hidden, false);
  assert.equal(nativeBtn.hidden, false);
  assert.equal(img.src, "/pilot/share.png?character=42");
  assert.equal(fetchCalls, 1, "imageBlob should start immediately on open without waiting for img.onload");

  // Simulate image finishing load
  img.complete = true;
  img.naturalWidth = 1200;
  img.onload();
  assert.equal(wait.textContent, "");
  assert.equal(dlg.classList.contains("share-loading"), false);

  await new Promise((r) => setTimeout(r, 10));
  assert.ok(env.api.ready["https://evesynapse.app/pilot/share.png?character=42"]);
});

test("reopening share dialog after an image load error retries loading cleanly", async () => {
  const env = makeStubEnv();
  const { dlg, img, wait } = env.makeDialog({ image: "/pilot/share.png?character=77" });

  env.setFetch(async () => ({ ok: false, status: 503 }));
  env.api.open(dlg);
  assert.equal(img.src, "/pilot/share.png?character=77");

  img.complete = true;
  img.naturalWidth = 0;
  img.onerror();
  assert.equal(wait.textContent, "The card could not be drawn just now.");
  assert.equal(img.getAttribute("src"), null);

  env.api.closeDialog(dlg);
  env.setFetch(async () => ({ ok: true, blob: async () => new Blob([new Uint8Array([1])], { type: "image/png" }) }));
  env.api.open(dlg);
  assert.equal(img.src, "/pilot/share.png?character=77");
});

test("copyImage passes ready Blob synchronously on Android and retries Promise-rejected ClipboardItem", async () => {
  const env = makeStubEnv();
  const { dlg, img, imgBtn } = env.makeDialog({ image: "/pilot/share.png?character=99" });

  const written = [];
  global.navigator.clipboard.write = async (items) => { written.push(items); };

  // Case 1: Ready blob already populated
  const readyBlob = new Blob([new Uint8Array([4, 5])], { type: "image/png" });
  env.api.ready["https://evesynapse.app/pilot/share.png?character=99"] = readyBlob;
  img.src = "/pilot/share.png?character=99";

  env.api.copyImage(imgBtn, img);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(written.length, 1);
  assert.equal(written[0][0].data["image/png"], readyBlob);
  assert.equal(imgBtn.textContent, "Image copied");

  // Case 2: Android/older Blink rejects Promise in ClipboardItem synchronously,
  // fallback waits for imageBlob and writes resolved Blob cleanly.
  const { dlg: dlg2, img: img2, imgBtn: imgBtn2 } = env.makeDialog({ id: "fit-share", image: "/fittings/public/fit/5/card.png" });
  const freshBlob = new Blob([new Uint8Array([7, 7])], { type: "image/png" });
  env.setFetch(async () => ({ ok: true, blob: async () => freshBlob }));

  const origClipboardItem = global.ClipboardItem;
  global.ClipboardItem = class StrictClipboardItem {
    constructor(data) {
      if (data["image/png"] && typeof data["image/png"].then === "function") {
        throw new TypeError("Value must be a Blob");
      }
      this.data = data;
    }
    static supports(m) { return m === "image/png"; }
  };
  img2.src = "/fittings/public/fit/5/card.png";
  env.api.copyImage(imgBtn2, img2);
  await new Promise((r) => setTimeout(r, 20));
  global.ClipboardItem = origClipboardItem;

  assert.equal(written.length, 2);
  assert.equal(written[1][0].data["image/png"], freshBlob);
  assert.equal(imgBtn2.textContent, "Image copied");
});

test("copyImage reports Copy failed cleanly on error and never disables button", async () => {
  const env = makeStubEnv();
  const { img, imgBtn } = env.makeDialog({ image: "/pilot/share.png?character=5" });
  env.api.ready["https://evesynapse.app/pilot/share.png?character=5"] = new Blob([new Uint8Array([1])], { type: "image/png" });
  img.src = "/pilot/share.png?character=5";

  global.navigator.clipboard.write = async () => { throw new Error("NotAllowedError"); };
  env.api.copyImage(imgBtn, img);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(imgBtn.textContent, "Copy failed");
  assert.equal(imgBtn.disabled, undefined);
});

test("shareCard falls back to title+url when file sharing rejects and flashes Share failed on error", async () => {
  const env = makeStubEnv();
  navigator.userAgent = "Mozilla/5.0 (Linux; Android 14) Chrome/126.0.0.0 Mobile Safari/537.36";
  const { dlg, img, nativeBtn } = env.makeDialog({
    image: "/pilot/share.png?character=12",
    link: "/pilot/?character=12",
  });
  env.api.ready["https://evesynapse.app/pilot/share.png?character=12"] = new Blob([new Uint8Array([1])], { type: "image/png" });
  img.src = "/pilot/share.png?character=12";

  const sharedPayloads = [];
  global.navigator.canShare = () => true;
  global.navigator.share = async (data) => {
    sharedPayloads.push(data);
    if (data.files) {
      const err = new Error("file share unsupported on desktop target");
      err.name = "DataError";
      throw err;
    }
  };

  env.api.shareCard(dlg, nativeBtn, img);
  await new Promise((r) => setTimeout(r, 10));

  assert.equal(sharedPayloads.length, 2);
  assert.ok(sharedPayloads[0].files);
  assert.deepEqual(sharedPayloads[1], {
    title: "Share pilot card",
    url: "https://evesynapse.app/pilot/?character=12",
  });

  // If sharing fails completely (not AbortError), button flashes "Share failed".
  global.navigator.share = async () => {
    const err = new Error("blocked");
    err.name = "NotAllowedError";
    throw err;
  };
  env.api.shareCard(dlg, nativeBtn, img);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(nativeBtn.textContent, "Share failed");
});

test("shareCard ignores user AbortError dismissal silently", async () => {
  const env = makeStubEnv();
  const { dlg, img, nativeBtn } = env.makeDialog({ image: "/pilot/share.png?character=13" });
  env.api.ready["https://evesynapse.app/pilot/share.png?character=13"] = new Blob([new Uint8Array([1])], { type: "image/png" });
  img.src = "/pilot/share.png?character=13";

  global.navigator.share = async () => {
    const err = new Error("user cancelled");
    err.name = "AbortError";
    throw err;
  };
  env.api.shareCard(dlg, nativeBtn, img);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(nativeBtn.textContent, "Share…");
});

test("backdrop click, close button, and content-will-swap close the <dialog> cleanly", () => {
  const env = makeStubEnv();
  const { dlg, closeBtn } = env.makeDialog();
  env.api.open(dlg);
  assert.equal(dlg.open, true);

  // Close button click
  env.dispatch("pointerdown", { target: closeBtn });
  env.dispatch("click", { target: closeBtn, clientX: 200, clientY: 200 });
  assert.equal(dlg.open, false);

  // Reopen, then click outside dialog bounds on ::backdrop
  env.api.open(dlg);
  assert.equal(dlg.open, true);
  env.dispatch("pointerdown", { target: dlg });
  env.dispatch("click", { target: dlg, clientX: 20, clientY: 20 });
  assert.equal(dlg.open, false);

  // Reopen, then drag from inside to backdrop must NOT close
  env.api.open(dlg);
  env.dispatch("pointerdown", { target: closeBtn });
  env.dispatch("click", { target: dlg, clientX: 20, clientY: 20 });
  assert.equal(dlg.open, true);

  // content-will-swap closes any open share <dialog>
  env.dispatch("evesynapse:content-will-swap", {});
  assert.equal(dlg.open, false);
});

test("blobToPng converts image/jpeg fallback cards to PNG via canvas and rejects non-image HTML", async () => {
  const env = makeStubEnv();
  const jpegBlob = new Blob([new Uint8Array([0xff, 0xd8, 0xff])], { type: "image/jpeg" });
  const pngBlob = await env.api.blobToPng(jpegBlob);
  assert.equal(pngBlob.type, "image/png");
  assert.equal(env.revokedUrls.length, 1);

  const htmlBlob = new Blob(["<html>error</html>"], { type: "text/html" });
  await assert.rejects(() => env.api.blobToPng(htmlBlob), /not an image/);

  env.setImageLoadMode("fail");
  await assert.rejects(() => env.api.blobToPng(jpegBlob), /decode failed/);
});

test("flash cancels prior per-button timer so rapid double taps keep their label", async () => {
  const env = makeStubEnv();
  const { linkBtn } = env.makeDialog();
  env.api.flash(linkBtn, "Copy failed");
  assert.equal(linkBtn.textContent, "Copy failed");
  await new Promise((r) => setTimeout(r, 15));
  env.api.flash(linkBtn, "Copied");
  assert.equal(linkBtn.textContent, "Copied");
  assert.equal(linkBtn.getAttribute("data-label"), "Copy link");
});

test("shareCard gives shared File a plain name without spaces for Android", async () => {
  const env = makeStubEnv();
  navigator.userAgent = "Mozilla/5.0 (Linux; Android 14) Chrome/126.0.0.0 Mobile Safari/537.36";
  const { dlg, img, nativeBtn, dlLink } = env.makeDialog({ fileName: "Kestrel Pilot - EveSynapse.png" });
  const shared = [];
  navigator.share = async (data) => { shared.push(data); };
  navigator.canShare = () => true;

  env.api.open(dlg);
  await new Promise((r) => setTimeout(r, 15));
  env.api.shareCard(dlg, nativeBtn, img);
  await new Promise((r) => setTimeout(r, 15));

  assert.equal(shared.length, 1);
  assert.equal(shared[0].files[0].name, "Kestrel-Pilot-EveSynapse.png");
  assert.equal(dlLink.getAttribute("download"), "Kestrel Pilot - EveSynapse.png");
});

test("open syncs html theme and vision onto /pilot/share.png URLs and redraws when theme changes", async () => {
  const env = makeStubEnv();
  const { dlg, img, dlLink } = env.makeDialog({ image: "/pilot/share.png?character=42" });

  // Default theme keeps the base URL unchanged.
  assert.equal(env.api.themedShareURL("/pilot/share.png?character=42"), "/pilot/share.png?character=42");
  assert.equal(env.api.themedShareURL("/fit/share.png?fit=9"), "/fit/share.png?fit=9");

  // LocalStorage / Settings scheme updates both preview src and download href on open.
  document.documentElement.setAttribute("data-scheme", "caldari");
  document.documentElement.setAttribute("data-vision", "redgreen");
  env.api.open(dlg);
  assert.equal(img.src, "/pilot/share.png?character=42&theme=caldari&vision=redgreen");
  assert.equal(dlLink.getAttribute("href"), "/pilot/share.png?character=42&download=1&theme=caldari&vision=redgreen");
  env.api.closeDialog(dlg);
});

test("canCopyImage hides Copy image on SamsungBrowser where PNG clipboard is unsupported", () => {
  const env = makeStubEnv();
  navigator.userAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 SamsungBrowser/25.0 Chrome/121.0.0.0 Mobile Safari/537.36";
  assert.equal(env.api.canCopyImage(), false);

  navigator.userAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/126.0.0.0 Mobile Safari/537.36";
  assert.equal(env.api.canCopyImage(), true);
});

test("canNativeShare respects navigator.canShare and desktop fine-pointer shares link synchronously", async () => {
  const env = makeStubEnv();
  const { dlg, img, nativeBtn } = env.makeDialog({ image: "/pilot/share.png?character=7", link: "/pilot/?character=7" });

  // Hidden if browser reports canShare=false for both URL and PNG file.
  navigator.canShare = () => false;
  assert.equal(env.api.canNativeShare(), false);

  // Shown when link sharing is supported; desktop fine-pointer skips file-share activation trap.
  navigator.canShare = (d) => !!(d && (d.url || (d.files && d.files.length)));
  assert.equal(env.api.canNativeShare(), true);

  env.api.ready["https://evesynapse.app/pilot/share.png?character=7"] = new Blob([new Uint8Array([1, 2])], { type: "image/png" });
  img.src = "/pilot/share.png?character=7";

  const shared = [];
  navigator.share = async (data) => { shared.push(data); };
  window.matchMedia = (q) => ({ matches: q === "(hover: hover) and (pointer: fine)", addEventListener() {} });

  env.api.shareCard(dlg, nativeBtn, img);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(shared.length, 1);
  assert.deepEqual(shared[0], { title: "Share pilot card", url: "https://evesynapse.app/pilot/?character=7" });
});

test("shareCard shares link synchronously when blob is not ready yet instead of awaiting fetch", async () => {
  const env = makeStubEnv();
  const { dlg, img, nativeBtn } = env.makeDialog({ image: "/pilot/share.png?character=8", link: "/pilot/?character=8" });
  img.src = "/pilot/share.png?character=8";

  const shared = [];
  let fetched = false;
  env.setFetch(async () => { fetched = true; return { ok: true, blob: async () => new Blob([new Uint8Array([1])], { type: "image/png" }) }; });
  navigator.canShare = () => true;
  navigator.share = async (data) => { shared.push(data); };

  env.api.shareCard(dlg, nativeBtn, img);
  assert.equal(shared.length, 1);
  assert.deepEqual(shared[0], { title: "Share pilot card", url: "https://evesynapse.app/pilot/?character=8" });
  assert.equal(fetched, false);
});
