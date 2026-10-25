export const ICONS = Object.freeze([
  { filename: "favicon-16x16.png", size: 16, label: "浏览器图标" },
  { filename: "favicon-32x32.png", size: 32, label: "浏览器图标" },
  { filename: "favicon-48x48.png", size: 48, label: "桌面图标" },
  { filename: "apple-touch-icon.png", size: 180, label: "Apple 触控图标" },
  { filename: "android-chrome-192x192.png", size: 192, label: "Android 图标" },
  { filename: "android-chrome-512x512.png", size: 512, label: "高清应用图标" },
]);

const controlCharacters = /[\u0000-\u001f\u007f-\u009f]/u;
const unpairedSurrogates = /[\ud800-\udfff]/u;
const onlyGoWhitespace = /^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$/u;
const byteLength = (value) => new TextEncoder().encode(value).length;

export class InputError extends Error {
  constructor(field, message) {
    super(message);
    this.name = "InputError";
    this.field = field;
  }
}

export function validateInput(fields) {
  const url = (fields.url ?? "").trim();
  if (!url || byteLength(url) > 4096 || controlCharacters.test(url) || /\s|\\/u.test(url) || unpairedSurrogates.test(url)) {
    throw new InputError("url", "请输入完整的 HTTP 或 HTTPS 图片地址，不要包含空白字符。");
  }
  let parsed;
  try {
    parsed = new URL(url);
  } catch {
    throw new InputError("url", "请输入有效的 HTTP 或 HTTPS 图片地址。");
  }
  const authority = url.match(/^https?:\/\/([^/?#]*)/iu);
  if (!authority || !["http:", "https:"].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password || authority[1].includes("@") || url.includes("#")) {
    throw new InputError("url", "图片地址须使用 HTTP 或 HTTPS，且不能包含账号、密码或片段。");
  }
  const signature = fields.signature ?? "";
  if (byteLength(signature) > 128 || controlCharacters.test(signature) || unpairedSurrogates.test(signature)) {
    throw new InputError("signature", "来源签名格式无效，请检查后重试。请勿填写签名私钥。");
  }
  const names = {};
  for (const [field, limit, label] of [["name", 128, "应用名称"], ["shortName", 32, "应用短名"]]) {
    const value = fields[field] === undefined || fields[field] === "" ? "App" : fields[field];
    if (onlyGoWhitespace.test(value) || unpairedSurrogates.test(value) || controlCharacters.test(value) || Array.from(value).length > limit) {
      throw new InputError(field, `${label}须为 1–${limit} 个字符，且不能包含控制字符。`);
    }
    names[field] = value;
  }
  // Beyond removing surrounding whitespace, preserve the source string:
  // URL.href normalization would change the bytes covered by a signature.
  return { url, signature, ...names };
}

export function buildIconURL(filename, config, includeNames = false) {
  if (filename !== "icons.zip" && !ICONS.some((icon) => icon.filename === filename)) {
    throw new Error("不支持的图标文件。");
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

export function messageForStatus(status) {
  switch (status) {
    case 400: return "参数无效，或图片格式暂不支持。请检查设置后重试。";
    case 401:
    case 403: return "图片来源不可访问，或来源签名无效。";
    case 404: return "找不到来源图片，请确认地址后重试。";
    case 413: return "图片超出服务资源限制，请换用较小的图片。";
    case 429: return "请求过于频繁，请稍后再试。";
    case 503: return "服务繁忙，请稍后重试。";
    case 504: return "处理超时，请稍后重试或换用较小的图片。";
    case 502: return "暂时无法读取或处理来源图片，请稍后重试。";
    default: return "生成失败，请稍后重试。";
  }
}

class RequestError extends Error {}

export function createApp({
  document,
  fetch: fetcher = globalThis.fetch.bind(globalThis),
  URL: objectURLs = globalThis.URL,
  AbortController: Abort = globalThis.AbortController,
  setTimeout: defer = globalThis.setTimeout,
}) {
  const get = (id) => document.getElementById(id);
  const form = get("icon-form");
  const fields = { url: get("source-url"), signature: get("source-signature"), name: get("app-name"), shortName: get("app-short-name") };
  const generateButton = get("generate-button");
  const cancelButton = get("cancel-button");
  const downloadButton = get("download-button");
  const status = get("status-message");
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
    generateButton.textContent = mode === "preview" ? "生成中…" : "生成预览 ↗";
    cancelButton.hidden = !mode;
    cancelButton.disabled = !mode;
    downloadButton.disabled = !!mode || !successful;
    downloadButton.textContent = mode === "download" ? "打包中…" : "下载图标包 ↓";
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
      card.state.textContent = "待生成";
    }
    setBusy(null);
  }

  function isCurrent(token, request) {
    return !destroyed && revision === token && !request.signal.aborted;
  }

  function options(request) {
    return { signal: request.signal, cache: "no-store", referrerPolicy: "no-referrer", credentials: "same-origin", mode: "same-origin", redirect: "error" };
  }

  async function getBlob(path, request, expectedType, invalidMessage) {
    const response = await fetcher(path, options(request));
    if (request.signal.aborted) throw new RequestError("请求已取消。");
    if (!response.ok) throw new RequestError(messageForStatus(response.status));
    const blob = await response.blob();
    if (!blob.size || blob.type.split(";", 1)[0].toLowerCase() !== expectedType) {
      throw new RequestError(invalidMessage);
    }
    return blob;
  }

  async function preview() {
    if (destroyed) return;
    clear();
    let config;
    try {
      config = validateInput(Object.fromEntries(Object.entries(fields).map(([key, field]) => [key, field.value])));
    } catch (error) {
      setStatus(error instanceof InputError ? error.message : "请检查输入后重试。", "error");
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
        activeCard.state.textContent = "生成中…";
        setStatus(`正在生成预览（${i + 1}/${cards.length}）…`, "loading");
        const blob = await getBlob(buildIconURL(activeCard.filename, config), request, "image/png", "预览返回了无效图片，请稍后重试。");
        if (!isCurrent(token, request)) return;
        const objectURL = objectURLs.createObjectURL(blob);
        blobs.add(objectURL);
        activeCard.image.src = objectURL;
        activeCard.image.hidden = false;
        if (typeof activeCard.image.decode === "function") await activeCard.image.decode();
        if (!isCurrent(token, request)) return;
        activeCard.state.textContent = "已就绪";
      }
      successful = Object.freeze({ ...config });
      setStatus("6 个尺寸已就绪，可以下载图标包了。", "success");
    } catch (error) {
      if (!isCurrent(token, request)) return;
      clear();
      if (activeCard) activeCard.state.textContent = "生成失败";
      setStatus(error instanceof RequestError ? error.message : "连接失败或图片无法显示，请检查网络后重试。", "error");
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
    setStatus("正在打包图标，请稍候…", "loading");
    try {
      const blob = await getBlob(buildIconURL("icons.zip", snapshot, true), request, "application/zip", "下载返回了无效文件，请稍后重试。");
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
      setStatus("图标包已准备，下载已开始。", "success");
    } catch (error) {
      if (!isCurrent(token, request)) return;
      setStatus(error instanceof RequestError ? error.message : "下载失败，请检查网络后重试。", "error");
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
    setStatus("已取消。确认图片地址后，可以重新生成预览。");
  }

  function onInput() {
    if (destroyed) return;
    clear();
    setStatus(fields.url.value ? "输入已更新，请重新生成预览。" : "粘贴图片地址，开始生成预览。");
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
  }

  form.addEventListener("submit", onSubmit);
  form.addEventListener("input", onInput);
  form.addEventListener("change", onInput);
  cancelButton.addEventListener("click", cancel);
  downloadButton.addEventListener("click", onDownload);
  setBusy(null);
  return { preview, download, cancel, destroy };
}

if (typeof document !== "undefined" && document.getElementById("icon-form")) {
  const app = createApp({ document });
  // BFCache may restore this same document without rerunning the module. Keep
  // handlers attached while releasing requests and blobs whenever it is hidden.
  globalThis.addEventListener("pagehide", () => app.cancel());
}
