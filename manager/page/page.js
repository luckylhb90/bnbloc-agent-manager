"use strict";
// The one place that talks to the component. In the app this page runs in a
// sandboxed frame with no network (the panel protocol, bnbloc
// docs/architecture/plugin-panels.md): the host forwards each call to the
// component with credentials the page never sees. Opened directly
// (development) the bearer is in the URL fragment.
const hostOrigin = new URLSearchParams(location.hash.slice(1)).get("host") || "";
const framed = window.parent !== window && hostOrigin !== "";
const token = new URLSearchParams(location.hash.slice(1)).get("token") || "";
let seq = 0; const pending = new Map();
if (framed) {
  window.addEventListener("message", (e) => {
    // Only the host, from the origin the frame URL names.
    if (e.source !== window.parent || e.origin !== hostOrigin || !e.data) return;
    const m = e.data;
    if (m.type === "panel.init") { document.documentElement.dataset.theme = m.theme; return; }
    if ((m.type !== "panel.result" && m.type !== "panel.refused") || !pending.has(m.id)) return;
    const p = pending.get(m.id); pending.delete(m.id);
    if (m.type === "panel.refused") return p.reject(new Error(m.reason));
    m.status >= 200 && m.status < 300 ? p.resolve(m.body) : p.reject(new Error((m.body && m.body.error) || "request failed (" + m.status + ")"));
  });
  window.parent.postMessage({ type: "panel.ready" }, "*");
}
function api(method, path, body) {
  if (framed) {
    return new Promise((resolve, reject) => {
      const id = ++seq; pending.set(id, { resolve, reject });
      window.parent.postMessage({ type: "panel.call", id, method, path, ...(body ? { body } : {}) }, "*");
    });
  }
  return fetch(path, { method, headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined })
    .then(async (r) => { const j = await r.json().catch(() => ({})); if (!r.ok) throw new Error(j.error || "request failed (" + r.status + ")"); return j; });
}

const $ = (id) => document.getElementById(id);
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") el.className = v; else if (k.startsWith("on")) el.addEventListener(k.slice(2), v); else if (v !== false && v != null) el.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) el.append(k.nodeType ? k : document.createTextNode(String(k)));
  return el;
}
const mb = (n) => n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : Math.max(1, Math.round(n / (1 << 20))) + " MB";

let snap = null, confirming = null, actionError = {}, timer = null;

async function act(id, action) {
  confirming = null; delete actionError[id];
  try { await api("POST", "/v1/agents/" + id + "/" + action); } catch (e) { actionError[id] = e.message; }
  await load();
}

function chip(a) {
  const op = a.op;
  if (op && op.running) return h("span", { class: "chip busy" }, { install: "Installing…", "install-sandbox": "Installing…", update: "Updating…", check: "Checking…" }[op.kind] || "Working…");
  if (a.status === "unsupported") return h("span", { class: "chip" }, "Not for this system");
  if (a.status === "blocked") return h("span", { class: "chip warn" }, "Not offered");
  if (op && op.error) return h("span", { class: "chip bad" }, "Failed");
  if (a.status === "absent") return h("span", { class: "chip" }, "Not installed");
  const v = a.managed ? a.managed.version : a.external && a.external.version;
  if (a.updateAvailable) return h("span", { class: "chip warn" }, (v ? v + " → " : "Update ") + a.target);
  if (a.updateUnknown) return h("span", { class: "chip" }, v ? "Installed " + v : "Installed");
  if (a.checkError && !a.latest) return h("span", { class: "chip" }, (v ? "Installed " + v + " · " : "") + "can’t check");
  return h("span", { class: "chip ok" }, v ? "Installed " + v : "Installed");
}

function detail(a) {
  const bits = [];
  if (a.managed) {
    bits.push((a.sandbox ? "In your sandbox, not on this computer" : "Managed here") + " · " + mb(a.managed.sizeBytes));
    if (a.managed.previous) bits.push("earlier version " + a.managed.previous + " kept");
    if (a.managed.held && a.managed.held.length) bits.push("kept for a room still using " + a.managed.held.join(", ") + " (removed when it no longer needs it)");
    if (a.pinned) bits.push("pinned by your organization to " + a.pinned);
  } else if (a.external) {
    const k = a.external.kind;
    bits.push("Found on this computer" + (k && k !== "unknown" ? " (" + k.replace("-", " ") + ")" : "") + ": " + a.external.path);
    if (a.pinned) bits.push("pinned by your organization to " + a.pinned);
  }
  if (a.source) bits.push("added by " + a.source);
  if (a.checkError && a.status === "installed") bits.push("couldn’t look for a newer version: " + a.checkError);
  return bits.length ? h("div", { class: "detail" }, bits.join(" · ")) : null;
}

function card(a) {
  const busy = a.op && a.op.running;
  const btns = [];
  const add = (label, action, primary) => btns.push(h("button", { class: primary ? "primary" : "", onclick: () => act(a.id, action) }, label));
  const both = a.can.install && a.can.installSandbox;
  if (a.can.install) {
    if (a.installCommand) btns.push(h("button", { class: "primary", onclick: () => { confirming = a.id + ":install"; render(); } }, both ? "Install on this computer" : "Install"));
    else add(both ? "Install on this computer" : "Install", "install", true);
  }
  if (a.can.installSandbox) add(a.can.install ? "Install in my sandbox" : "Install", "install-sandbox", !a.can.install);
  if (a.can.update) {
    if (a.external && a.updateCommand) btns.push(h("button", { class: "primary", onclick: () => { confirming = a.id; render(); } }, a.updateUnknown ? "Run its updater" : "Update"));
    else add("Update", "update", true);
  }
  if (a.can.rollback) add("Roll back", "rollback");
  if (a.can.remove) add("Remove", "remove");
  if (busy) btns.push(h("button", { onclick: () => act(a.id, "cancel") }, "Cancel"));

  const body = [h("div", { class: "row" }, h("div", { class: "name" }, a.name), chip(a), btns)];
  if (a.can.register || a.can.unregister) {
    body.push(h("label", { class: "toggle" }, h("input", { type: "checkbox", checked: a.registered, onchange: () => act(a.id, a.registered ? "unregister" : "register") }), "Use in rooms"));
  }
  if (a.shadowed && (a.can.register || a.can.unregister || a.registered)) {
    body.push(h("div", { class: "hint" }, "Your own agents.json already defines this agent for rooms, so rooms run that one, not this. Remove its entry there to use this."));
  }
  if (!(a.can.register || a.can.unregister) && !a.hasACP && a.status === "installed") {
    body.push(h("div", { class: "hint" }, a.adapter ? "Rooms run it through the ACP adapter below." : "Kept current here; rooms can’t run it yet (no ACP launch line)."));
  }
  body.push(detail(a));
  if (confirming === a.id + ":install") {
    body.push(h("div", { class: "hint" }, "This runs on your computer:",
      h("code", { class: "cmd" }, a.installCommand),
      h("button", { class: "primary", onclick: () => act(a.id, "install") }, "Run"), " ", h("button", { onclick: () => { confirming = null; render(); } }, "Cancel")));
  }
  if (confirming === a.id) {
    body.push(h("div", { class: "hint" }, "This runs on your computer, changing your own install", a.external && a.external.kind === "npm-global" ? "" : " (the manager cannot roll it back)", ":",
      h("code", { class: "cmd" }, a.updateCommand),
      h("button", { class: "primary", onclick: () => act(a.id, "update") }, "Run"), " ", h("button", { onclick: () => { confirming = null; render(); } }, "Cancel")));
  }
  if (a.reason && !(a.can.install || a.can.update) && a.status !== "unsupported") body.push(h("div", { class: "hint" }, a.reason));
  if (a.sandboxOffer && a.can.installSandbox) {
    const o = a.sandboxOffer;
    body.push(h("div", { class: "hint" }, "In your sandbox: nothing of it is installed or run on this computer, and every room shares the one copy" + (o.approxMB ? " (about " + o.approxMB + " MB)" : "") + ". It works with free models without signing in; signing in to a provider from the sandbox is not available yet. " + (o.note || "")));
  }
  if (a.status === "absent" && a.installHint && !a.can.installSandbox) body.push(h("div", { class: "hint" }, a.installHint));
  if (a.status === "installed" && a.signIn && a.managed && !busy) body.push(h("div", { class: "hint" }, a.signIn));
  if (actionError[a.id]) body.push(h("div", { class: "err" }, actionError[a.id]));
  if (a.op && a.op.error && !busy) body.push(h("div", { class: "err" }, a.op.error));
  if (a.op && a.op.log && a.op.log.length) body.push(h("details", { open: busy || !!a.op.error }, h("summary", {}, busy ? "Progress" : "Last operation"), h("pre", {}, a.op.log.join("\n"))));
  return h("div", { class: a.for ? "card sub" : "card", id: "a-" + a.id }, body);
}

function render() {
  if (!snap) return;
  const openLogs = new Set([...document.querySelectorAll("details[open]")].map((d) => d.closest(".card") && d.closest(".card").id));
  $("notes").replaceChildren(...(snap.notes || []).map((n) => h("div", { class: "notes" }, n)));
  const shown = snap.agents.filter((a) => a.status !== "unsupported");
  // An adapter row follows the agent it serves.
  const ordered = shown.filter((a) => !a.for).flatMap((a) => [a, ...shown.filter((c) => c.for === a.id)]);
  $("list").replaceChildren(...ordered.map(card));
  for (const d of document.querySelectorAll(".card")) if (openLogs.has(d.id)) { const x = d.querySelector("details"); if (x) x.open = true; }
  const s = snap.settings;
  $("foot").textContent = "Agent manager " + snap.component + " · catalog " + snap.catalogVersion +
    (s.holdDays ? " · new versions are offered after " + s.holdDays + " days" : "") + (s.autoUpdate ? " · automatic updates on" : "");
}

async function load() {
  try { snap = await api("GET", "/v1/state"); } catch (e) { $("list").replaceChildren(h("div", { class: "err" }, "The agent manager is not answering: " + e.message)); return; }
  render();
  clearTimeout(timer);
  const busy = snap.detecting || snap.agents.some((a) => a.op && a.op.running);
  timer = setTimeout(load, busy ? 1200 : 8000);
}

$("refresh").addEventListener("click", async () => {
  $("refresh").disabled = true;
  try { snap = await api("POST", "/v1/refresh"); render(); } catch (e) { $("notes").replaceChildren(h("div", { class: "notes" }, e.message)); }
  $("refresh").disabled = false; load();
});
document.addEventListener("visibilitychange", () => { if (!document.hidden) load(); });
load();
