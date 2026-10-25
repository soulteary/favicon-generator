export const ICONS = Object.freeze([
  { filename: "favicon-16x16.png", size: 16, labelKey: "icon.16.label" },
  { filename: "favicon-32x32.png", size: 32, labelKey: "icon.32.label" },
  { filename: "favicon-48x48.png", size: 48, labelKey: "icon.48.label" },
  { filename: "apple-touch-icon.png", size: 180, labelKey: "icon.180.label" },
  { filename: "android-chrome-192x192.png", size: 192, labelKey: "icon.192.label" },
  { filename: "android-chrome-512x512.png", size: 512, labelKey: "icon.512.label" },
]);

const identityTranslation = (key) => key;

export function createTranslator(catalog) {
  if (!catalog || typeof catalog !== "object" || Array.isArray(catalog) || !Object.keys(catalog).length || Object.values(catalog).some((value) => typeof value !== "string" || !value.trim())) {
    throw new Error("Invalid translation catalog");
  }
  const messages = Object.freeze({ ...catalog });
  return (key, params = {}) => {
    const text = Object.hasOwn(messages, key) ? messages[key] : messages["error.generic"];
    if (typeof text !== "string") throw new Error("Missing translation");
    return text.replace(/\{([a-zA-Z0-9_]+)\}/g, (match, name) => Object.hasOwn(params, name) ? String(params[name]) : match);
  };
}

export function applyTranslations(document, translate, locale) {
  if (document.documentElement) document.documentElement.lang = locale;
  document.title = translate("page.title");
  for (const [attribute, target] of [["data-i18n", null], ["data-i18n-alt", "alt"], ["data-i18n-aria-label", "aria-label"], ["data-i18n-placeholder", "placeholder"]]) {
    for (const node of document.querySelectorAll?.(`[${attribute}]`) ?? []) {
      const message = translate(node.getAttribute(attribute));
      if (target) node.setAttribute(target, message);
      else node.textContent = message;
    }
  }
  const selector = document.getElementById("language-select");
  if (selector) selector.value = locale;
}

const controlCharacters = /[\u0000-\u001f\u007f-\u009f]/u;
const unpairedSurrogates = /[\ud800-\udfff]/u;
const onlyGoWhitespace = /^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$/u;
const byteLength = (value) => new TextEncoder().encode(value).length;

export class InputError extends Error {
  constructor(field, code, params = {}, translate = identityTranslation) {
    super(translate(code, params));
    this.code = code;
    this.params = params;
    this.name = "InputError";
    this.field = field;
  }
}

export function validateInput(fields, translate = identityTranslation) {
  const url = (fields.url ?? "").trim();
  if (!url || byteLength(url) > 4096 || controlCharacters.test(url) || /\s|\\/u.test(url) || unpairedSurrogates.test(url)) {
    throw new InputError("url", "validation.url", {}, translate);
  }
  let parsed;
  try {
    parsed = new URL(url);
  } catch {
    throw new InputError("url", "validation.url_syntax", {}, translate);
  }
  const authority = url.match(/^https?:\/\/([^/?#]*)/iu);
  if (!authority || !["http:", "https:"].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password || authority[1].includes("@") || url.includes("#")) {
    throw new InputError("url", "validation.url_policy", {}, translate);
  }
  const signature = fields.signature ?? "";
  if (byteLength(signature) > 128 || controlCharacters.test(signature) || unpairedSurrogates.test(signature)) {
    throw new InputError("signature", "validation.signature", {}, translate);
  }
  const names = {};
  for (const [field, limit, label] of [["name", 128, "form.name.label"], ["shortName", 32, "form.shortName.label"]]) {
    const value = fields[field] === undefined || fields[field] === "" ? "App" : fields[field];
    if (onlyGoWhitespace.test(value) || unpairedSurrogates.test(value) || controlCharacters.test(value) || Array.from(value).length > limit) {
      throw new InputError(field, "validation.name", { label: translate(label), limit }, translate);
    }
    names[field] = value;
  }
  // Beyond removing surrounding whitespace, preserve the source string:
  // URL.href normalization would change the bytes covered by a signature.
  return { url, signature, ...names };
}

export function buildIconURL(filename, config, includeNames = false) {
  if (filename !== "icons.zip" && !ICONS.some((icon) => icon.filename === filename)) {
    throw new Error("Unsupported icon file");
  }
  const query = new URLSearchParams();
  query.set("url", config.url);
  if (config.signature) query.set("s", config.signature);
  if (includeNames) {
    query.set("name", config.name);
    query.set("shortName", config.shortName);
  }
  return `/icons/${filename}?${query.toString()}`;
}

export function errorCodeForStatus(status) {
  switch (status) {
    case 400: return "invalid_parameters";
    case 401:
    case 403: return "source_forbidden";
    case 404: return "source_not_found";
    case 413: return "resource_limit";
    case 429: return "rate_limited";
    case 503: return "busy";
    case 504: return "timeout";
    case 502: return "upstream_failure";
    default: return "generic";
  }
}

export function messageForStatus(status, translate = identityTranslation) {
  return translate(`error.${errorCodeForStatus(status)}`);
}

const apiErrorCodes = new Set(["invalid_parameters", "source_forbidden", "source_not_found", "resource_limit", "rate_limited", "busy", "timeout", "upstream_failure", "generic"]);

class RequestError extends Error {
  constructor(code, translate) {
    super(translate(`error.${code}`));
    this.code = code;
  }
}

export function createApp({
  document,
  fetch: fetcher = globalThis.fetch.bind(globalThis),
  URL: objectURLs = globalThis.URL,
  AbortController: Abort = globalThis.AbortController,
  setTimeout: defer = globalThis.setTimeout,
  translate = identityTranslation,
  catalogs = null,
  locale = document.documentElement?.lang || "zh-CN",
}) {
  let t = translate;
  const get = (id) => document.getElementById(id);
  const form = get("icon-form");
  const fields = { url: get("source-url"), signature: get("source-signature"), name: get("app-name"), shortName: get("app-short-name") };
  const generateButton = get("generate-button");
  const cancelButton = get("cancel-button");
  const downloadButton = get("download-button");
  const status = get("status-message");
  const languageSelect = get("language-select");
  const cards = ICONS.map((icon) => ({ ...icon, image: get(`image-${icon.size}`), state: get(`state-${icon.size}`) }));
  const blobs = new Set();
  let revision = 0;
  let controller = null;
  let successful = null;
  let destroyed = false;

  function setStatus(message, tone = "idle") {
    status.textContent = message;
    status.className = `status-message ${tone}`;
  }

  function release(url) {
    if (blobs.delete(url)) objectURLs.revokeObjectURL(url);
  }

  function setBusy(mode) {
    generateButton.disabled = mode === "preview";
    generateButton.textContent = t(mode === "preview" ? "action.generating" : "action.generate");
    cancelButton.hidden = !mode;
    cancelButton.disabled = !mode;
    downloadButton.disabled = !!mode || !successful;
    downloadButton.textContent = t(mode === "download" ? "action.packing" : "action.download");
    form.setAttribute("aria-busy", mode === "preview" ? "true" : "false");
  }

  function clear() {
    revision++;
    if (controller) controller.abort();
    controller = null;
    successful = null;
    for (const url of blobs) release(url);
    for (const card of cards) {
      card.image.removeAttribute("src");
      card.image.hidden = true;
      card.state.textContent = t("preview.pending");
    }
    setBusy(null);
  }

  function isCurrent(token, request) {
    return !destroyed && revision === token && !request.signal.aborted;
  }

  function options(request) {
    return { signal: request.signal, cache: "no-store", referrerPolicy: "no-referrer", credentials: "same-origin", mode: "same-origin", redirect: "error" };
  }

  async function getBlob(path, request, expectedType, invalidCode) {
    const response = await fetcher(path, options(request));
    if (request.signal.aborted) throw new RequestError("generic", t);
    if (!response.ok) {
      const supplied = response.headers?.get("X-Favicon-Error-Code");
      throw new RequestError(apiErrorCodes.has(supplied) ? supplied : errorCodeForStatus(response.status), t);
    }
    const blob = await response.blob();
    if (!blob.size || blob.type.split(";", 1)[0].toLowerCase() !== expectedType) {
      throw new RequestError(invalidCode, t);
    }
    return blob;
  }

  async function preview() {
    if (destroyed) return;
    clear();
    let config;
    try {
      config = validateInput(Object.fromEntries(Object.entries(fields).map(([key, field]) => [key, field.value])), t);
    } catch (error) {
      setStatus(error instanceof InputError ? error.message : t("error.input"), "error");
      if (error instanceof InputError) {
        const advanced = get("advanced-settings");
        if (error.field !== "url" && advanced) advanced.open = true;
        fields[error.field].focus();
      }
      return;
    }
    const token = revision;
    const request = new Abort();
    controller = request;
    setBusy("preview");
    let activeCard;
    try {
      // One request at a time leaves capacity for other visitors and avoids
      // submitting six conversions against the default four-request limit.
      for (let i = 0; i < cards.length; i++) {
        activeCard = cards[i];
        activeCard.state.textContent = t("preview.generating");
        setStatus(t("status.progress", { current: i + 1, total: cards.length }), "loading");
        const blob = await getBlob(buildIconURL(activeCard.filename, config), request, "image/png", "invalid_image");
        if (!isCurrent(token, request)) return;
        const objectURL = objectURLs.createObjectURL(blob);
        blobs.add(objectURL);
        activeCard.image.src = objectURL;
        activeCard.image.hidden = false;
        if (typeof activeCard.image.decode === "function") await activeCard.image.decode();
        if (!isCurrent(token, request)) return;
        activeCard.state.textContent = t("preview.ready");
      }
      successful = Object.freeze({ ...config });
      setStatus(t("status.ready"), "success");
    } catch (error) {
      if (!isCurrent(token, request)) return;
      clear();
      if (activeCard) activeCard.state.textContent = t("preview.failed");
      setStatus(error instanceof RequestError ? error.message : t("error.network"), "error");
      status.focus();
    } finally {
      if (isCurrent(token, request)) {
        controller = null;
        setBusy(null);
      }
    }
  }

  async function download() {
    if (destroyed || !successful) return;
    const snapshot = successful;
    if (controller) controller.abort();
    const token = ++revision;
    const request = new Abort();
    controller = request;
    setBusy("download");
    setStatus(t("status.packing"), "loading");
    try {
      const blob = await getBlob(buildIconURL("icons.zip", snapshot, true), request, "application/zip", "invalid_archive");
      if (!isCurrent(token, request)) return;
      const objectURL = objectURLs.createObjectURL(blob);
      blobs.add(objectURL);
      const anchor = document.createElement("a");
      anchor.href = objectURL;
      anchor.download = "icons.zip";
      anchor.referrerPolicy = "no-referrer";
      try {
        document.body.appendChild(anchor);
        anchor.click();
      } finally {
        anchor.remove();
        defer(() => release(objectURL), 1000);
      }
      setStatus(t("status.downloaded"), "success");
    } catch (error) {
      if (!isCurrent(token, request)) return;
      setStatus(error instanceof RequestError ? error.message : t("error.download"), "error");
      status.focus();
    } finally {
      if (isCurrent(token, request)) {
        controller = null;
        setBusy(null);
      }
    }
  }

  function cancel() {
    if (destroyed) return;
    clear();
    setStatus(t("status.canceled"));
  }

  function onInput() {
    if (destroyed) return;
    clear();
    setStatus(t(fields.url.value ? "status.updated" : "status.idle"));
  }

  function onSubmit(event) {
    event.preventDefault();
    void preview();
  }

  function onDownload(event) {
    event.preventDefault();
    void download();
  }

  function destroy() {
    if (destroyed) return;
    clear();
    destroyed = true;
    form.removeEventListener("submit", onSubmit);
    form.removeEventListener("input", onInput);
    form.removeEventListener("change", onInput);
    cancelButton.removeEventListener("click", cancel);
    downloadButton.removeEventListener("click", onDownload);
    languageSelect?.removeEventListener("change", onLanguageChange);
  }

  function setLanguage(value) {
    if (destroyed || !catalogs || !["zh-CN", "en"].includes(value)) return;
    clear();
    locale = value;
    t = createTranslator(catalogs[value]);
    applyTranslations(document, t, value);
    for (const card of cards) card.state.textContent = t("preview.pending");
    setBusy(null);
    setStatus(t("status.idle"));
  }

  function onLanguageChange() { setLanguage(languageSelect.value); }

  languageSelect?.addEventListener("change", onLanguageChange);
  if (languageSelect) languageSelect.disabled = false;
  applyTranslations(document, t, locale);
  form.addEventListener("submit", onSubmit);
  form.addEventListener("input", onInput);
  form.addEventListener("change", onInput);
  cancelButton.addEventListener("click", cancel);
  downloadButton.addEventListener("click", onDownload);
  setBusy(null);
  return { preview, download, cancel, destroy, setLanguage };
}

export async function bootstrap({ document, fetch: fetcher = globalThis.fetch.bind(globalThis), signal, ...dependencies }) {
  const catalogs = {};
  for (const locale of ["zh-CN", "en"]) {
    if (signal?.aborted) throw new Error("Translations canceled");
    const response = await fetcher(`/assets/locales/${locale}.json`, {
      signal, cache: "no-store", credentials: "same-origin", mode: "same-origin", redirect: "error", referrerPolicy: "no-referrer",
    });
    if (signal?.aborted || !response.ok) throw new Error("Translations unavailable");
    catalogs[locale] = await response.json();
    if (signal?.aborted) throw new Error("Translations canceled");
    createTranslator(catalogs[locale]);
  }
  if (signal?.aborted) throw new Error("Translations canceled");
  const locale = document.documentElement?.lang === "en" ? "en" : "zh-CN";
  return createApp({ document, ...dependencies, fetch: fetcher, catalogs, locale, translate: createTranslator(catalogs[locale]) });
}

if (typeof document !== "undefined" && document.getElementById("icon-form")) {
  let app = null;
  let loading = null;
  const start = () => {
    if (app || loading && !loading.signal.aborted) return;
    const request = new AbortController();
    loading = request;
    void bootstrap({ document, signal: request.signal }).then((created) => {
      if (request.signal.aborted) created.destroy();
      else app = created;
    }).catch(() => {
      if (!request.signal.aborted) {
        document.getElementById("status-message").textContent = document.getElementById("catalog-error-message").textContent;
      }
    }).finally(() => { if (loading === request) loading = null; });
  };
  globalThis.addEventListener("pagehide", () => { loading?.abort(); app?.cancel(); });
  globalThis.addEventListener("pageshow", start);
  start();
}
