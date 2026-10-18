"use strict";

// Every piece of user text reaches the page through textContent, never markup.
function el(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) {
    if (k == null || k === false) continue;
    n.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return n;
}

function $(id) { return document.getElementById(id); }

async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: "same-origin" };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  if (r.status === 401) { location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search); throw new Error("login"); }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || "gagal (" + r.status + ")");
  return data;
}

function pad(n) { return String(n).padStart(2, "0"); }

function hhmm(t) {
  const d = new Date(t);
  return d.toLocaleTimeString("id-ID", { hour: "2-digit", minute: "2-digit", hour12: false, timeZone: "Asia/Jakarta" }).replace(".", ":");
}

function left(ms) {
  if (ms <= 0) return "00:00:00";
  const s = Math.floor(ms / 1000);
  return pad(Math.floor(s / 3600)) + ":" + pad(Math.floor(s / 60) % 60) + ":" + pad(s % 60);
}

function lineText(l) {
  let s = l.menu;
  if (l.sate > 0) s += " " + l.sate + " SATE";
  if (l.note) s += " (" + l.note + ")";
  return s;
}

function porsi(t) {
  return t.bowls + " porsi" + (t.sate ? ", " + t.sate + " sate" : "");
}

// phase reads the order against the server clock, so the page flips at the
// lock second even before the server's own tick reports it.
function phase(o, now) {
  if (o.status === "cancelled") return "cancelled";
  if (o.status === "closed") return "closed";
  const lock = Date.parse(o.lock_at), late = Date.parse(o.late_until);
  if (now < lock) return "open";
  if (now < late) return "late";
  return "closed";
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); return true; }
  catch (_) {
    const t = el("textarea", { readonly: true }, text);
    document.body.append(t); t.select();
    const ok = document.execCommand("copy"); t.remove(); return ok;
  }
}

async function whoami() {
  const me = await api("GET", "/api/me");
  const who = $("who");
  if (who) who.replaceChildren(me.name, " · ", el("a", { href: "/auth/logout" }, "keluar"));
  if (me.acting_as) {
    const stop = async () => { await api("POST", "/api/impersonate/stop", {}); location.reload(); };
    document.querySelector("main").prepend(el("div", { class: "as-bar", role: "status" },
      el("span", {}, "🎭 Sebagai " + me.name),
      el("button", { type: "button", class: "link", onclick: stop }, "kembali ke " + me.real_name)));
  }
  return me;
}
