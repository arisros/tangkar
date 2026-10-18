"use strict";

const id = location.pathname.split("/").pop();
let me = null, snap = null, offset = 0, menu = "DAGING", sate = 0, busy = false;

const now = () => Date.now() + offset;

function setMenu(m) {
  menu = m;
  for (const k of document.querySelectorAll(".key")) k.setAttribute("aria-pressed", k.dataset.menu === m ? "true" : "false");
  goLabel();
}

function setSate(n) {
  sate = Math.max(0, Math.min(10, n));
  $("sate").textContent = sate;
  $("sate-minus").disabled = sate === 0;
  $("sate-plus").disabled = sate === 10;
  goLabel();
}

function goLabel() {
  const p = snap ? phase(snap.order, now()) : "open";
  let s = (p === "late" ? "Pesan susulan: " : "Pesan ") + menu;
  if (sate) s += " + " + sate + " sate";
  $("go").textContent = s;
}

async function order() {
  if (busy) return;
  busy = true; $("go").disabled = true; $("err").textContent = "";
  try {
    await api("POST", "/api/orders/" + id + "/lines", { menu, sate, note: $("note").value });
    $("note").value = ""; $("note-count").textContent = "0/10"; setSate(0);
  } catch (e) { $("err").textContent = e.message; }
  finally { busy = false; $("go").disabled = false; }
}

async function del(lineID) {
  try { await api("DELETE", "/api/orders/" + id + "/lines/" + lineID); }
  catch (e) { $("err").textContent = e.message; }
}

function sendState(s) {
  if (!s) return null;
  if (s.status === "sent") return el("span", { class: "ok" }, "✓ terkirim " + hhmm(s.sent_at * 1000));
  if (s.status === "pending") return el("span", { class: "warn" }, "mengirim…");
  if (s.status === "failed") return el("span", { class: "bad" }, "gagal, dicoba lagi");
  return null;
}

function renderNota() {
  const body = $("nota-body");
  const lines = snap.lines;
  if (!lines.length) { body.replaceChildren(el("p", { class: "empty" }, "belum ada pesanan")); return; }
  const groups = new Map();
  for (const l of lines) {
    const k = l.batch == null ? "wait" : l.batch;
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(l);
  }
  const keys = [...groups.keys()].filter(k => k !== "wait").sort((a, b) => a - b);
  if (groups.has("wait")) keys.push("wait");
  const sends = new Map(snap.sends.map(s => [s.batch, s]));
  let n = 0;
  const parts = keys.map(k => {
    let title;
    if (k === "wait") title = "Susulan, menunggu";
    else title = n++ === 0 ? "Pesanan " + hhmm(snap.order.lock_at) : "Tambahan #" + (n - 1);
    let status = k === "wait"
      ? el("span", { class: "warn" }, snap.cut_at ? "ke Mamang ±" + hhmm(snap.cut_at) : "")
      : sendState(sends.get(k));
    if (k === 0 && phase(snap.order, now()) === "open") status = el("span", {}, "dikirim " + hhmm(snap.order.lock_at));
    return el("div", { class: "sec" },
      el("div", { class: "sec-h" }, el("span", {}, title.toUpperCase()), status),
      groups.get(k).map(l => el("div", { class: "ln" }, el("span", {}, lineText(l)), el("span", { class: "nm" }, l.name))));
  });
  const t = snap.totals;
  parts.push(el("div", { class: "tot" },
    el("div", { class: "ln" }, el("span", {}, "DAGING " + t.daging + "  CAMPUR " + t.campur), el("span", {}, "SATE " + t.sate)),
    el("div", { class: "ln big" }, el("span", {}, "TOTAL"), el("span", {}, porsi(t)))));
  body.replaceChildren(...parts);
}

function canDelete(l, p) {
  if (!l.late) return p === "open";
  return l.batch == null && p !== "cancelled";
}

function renderMine(p) {
  const mine = snap.lines.filter(l => l.email === me.email);
  $("mine").hidden = mine.length === 0;
  $("mine-list").replaceChildren(...mine.map(l => el("li", {},
    el("span", {}, lineText(l),
      l.late ? el("span", { class: "tag late" }, "susulan") : null,
      l.batch != null && (snap.sends.find(s => s.batch === l.batch) || {}).status === "sent" ? el("span", { class: "tag sent" }, "terkirim") : null),
    canDelete(l, p) ? el("button", { class: "del", type: "button", onclick: () => del(l.id) }, "Hapus") : null)));
}

function renderBoard() {
  if (!snap) return;
  const o = snap.order, t = now(), p = phase(o, t);
  const b = $("board");
  b.className = "board" + (p === "late" ? " late" : "") + (p === "closed" || p === "cancelled" ? " done" : "");
  const lock = Date.parse(o.lock_at), late = Date.parse(o.late_until);
  if (p === "open") {
    $("b-label").textContent = "Tutup dalam";
    $("b-clock").textContent = left(lock - t);
    $("b-state").textContent = "Buka";
    $("b-at").textContent = "tutup " + hhmm(lock);
    if (lock - t < 5 * 60 * 1000) b.classList.add("urgent");
  } else if (p === "late") {
    $("b-label").textContent = "Late join sisa";
    $("b-clock").textContent = left(late - t);
    $("b-state").textContent = "Late join";
    $("b-at").textContent = "s.d. " + hhmm(late);
  } else {
    $("b-label").textContent = p === "cancelled" ? "Dibatalkan" : "Sudah tutup";
    $("b-clock").textContent = hhmm(lock);
    $("b-state").textContent = p === "cancelled" ? "Batal" : "Tutup";
    $("b-at").textContent = "";
  }
  const bar = $("late-bar");
  bar.hidden = p !== "late";
  if (p === "late") bar.textContent = "Kakak-kakak, pesanan utama sudah jalan ke Mamang. Pesanan Kakak sekarang masuk TAMBAHAN, dikirim otomatis 2 menit setelah susulan terakhir (paling lama 4 menit).";
  $("machine").hidden = false;
  $("stamp").hidden = p === "open" || p === "late";
  for (const x of $("machine").querySelectorAll("button, input")) x.disabled = !(p === "open" || p === "late") || (x.id === "go" && busy);
  if (p === "open" || p === "late") setSate(sate);
  goLabel();
  if (b.dataset.phase !== p) {
    b.dataset.phase = p;
    renderMine(p); renderNota(); renderAdmin(p);
  }
}

function renderAdmin(p) {
  if (!me.admin) return;
  $("admin").hidden = false;
  $("a-lock").hidden = p !== "open";
  $("a-cancel").hidden = p !== "open";
  $("a-late-off").hidden = p !== "late";
  $("a-time").parentElement.hidden = p !== "open";
  $("a-late").parentElement.hidden = !(p === "open" || p === "late");
  $("a-time").value = hhmm(snap.order.lock_at);
  $("a-late").value = Math.round((Date.parse(snap.order.late_until) - Date.parse(snap.order.lock_at)) / 60000);
}

// renderSendBar tells everyone what Mamang has: on its way, arrived, or not yet.
function renderSendBar() {
  const bar = $("send-bar");
  const sends = snap.sends.filter(s => s.status !== "skipped");
  const last = sends[sends.length - 1];
  bar.hidden = !last;
  if (!last) return;
  if (last.status === "pending") {
    bar.className = "send-bar sending";
    bar.replaceChildren(el("span", {}, "🛵"), " Lagi dikirim ke Mamang! Sebentar ya Kakak 🙏");
  } else if (last.status === "failed") {
    bar.className = "send-bar fail";
    bar.textContent = "Mamang belum nerima, lagi dicoba lagi ya Kakak 🙏";
  } else if (now() - last.sent_at * 1000 < 15 * 60 * 1000) {
    bar.className = "send-bar done";
    bar.textContent = "✓ Sudah sampai ke Mamang, Kakak-kakak 😋 (" + hhmm(last.sent_at * 1000) + ")";
  } else {
    bar.hidden = true;
  }
}

function render() {
  const o = snap.order;
  document.title = o.title + " · Tangkar";
  $("title").textContent = o.title;
  $("day").textContent = o.day_label;
  const v = snap.viewers;
  $("viewers").textContent = v.length ? "Lagi buka: " + v.slice(0, 4).join(", ") + (v.length > 4 ? " +" + (v.length - 4) : "") : "";
  const p = phase(o, now());
  $("board").dataset.phase = p;
  renderBoard(); renderMine(p); renderNota(); renderAdmin(p); renderSendBar();
}

function renderPhone() {
  let skipped = false;
  try { skipped = localStorage.getItem("tangkar.phone.skip") === "1"; } catch (_) {}
  $("phone").hidden = !!me.phone || skipped;
  $("phone-on").textContent = me.phone ? " · notif WA ke " + me.phone : "";
}

async function phoneCode() {
  $("phone-err").textContent = "";
  try {
    await api("POST", "/api/phone/code", { phone: $("phone-in").value });
    $("phone-step1").hidden = true; $("phone-step2").hidden = false;
    $("phone-msg").textContent = "Kode sudah dikirim ke WhatsApp Kakak, masukkan di sini ya.";
    $("code-in").focus();
  } catch (e) { $("phone-err").textContent = e.message; }
}

async function phoneVerify() {
  $("phone-err").textContent = "";
  try {
    const r = await api("POST", "/api/phone/verify", { code: $("code-in").value });
    me.phone = r.phone; renderPhone();
  } catch (e) { $("phone-err").textContent = e.message; }
}

async function adminDo(path, body, ask) {
  if (ask && !confirm(ask)) return;
  $("a-err").textContent = "";
  try { await api("POST", "/api/orders/" + id + path, body); }
  catch (e) { $("a-err").textContent = e.message; }
}

function connect() {
  const es = new EventSource("/api/orders/" + id + "/events");
  es.addEventListener("snapshot", ev => {
    snap = JSON.parse(ev.data);
    offset = snap.now - Date.now();
    render();
  });
  es.onopen = () => { $("live-dot").className = "dot on"; $("live").textContent = "live"; };
  es.onerror = () => { $("live-dot").className = "dot"; $("live").textContent = "menyambung ulang"; };
}

document.addEventListener("DOMContentLoaded", async () => {
  for (const k of document.querySelectorAll(".key")) k.addEventListener("click", () => setMenu(k.dataset.menu));
  $("sate-minus").addEventListener("click", () => setSate(sate - 1));
  $("sate-plus").addEventListener("click", () => setSate(sate + 1));
  $("note").addEventListener("input", e => { $("note-count").textContent = [...e.target.value].length + "/10"; });
  $("note").addEventListener("keydown", e => { if (e.key === "Enter") order(); });
  $("go").addEventListener("click", order);
  $("a-copy").addEventListener("click", async () => { $("a-copy").textContent = (await copy(location.origin + "/o/" + id)) ? "Tersalin ✓" : "Gagal salin"; });
  $("a-lock").addEventListener("click", () => adminDo("/lock", {}, "Kunci sekarang dan kirim ke grup?"));
  $("a-cancel").addEventListener("click", () => adminDo("/cancel", {}, "Batalkan pesanan ini? Tidak ada yang dikirim."));
  $("a-late-off").addEventListener("click", () => adminDo("/late", { minutes: 0 }, "Tutup late join sekarang?"));
  $("a-time-go").addEventListener("click", () => adminDo("/lock", { at: $("a-time").value }));
  $("a-late-go").addEventListener("click", () => adminDo("/late", { minutes: parseInt($("a-late").value, 10) || 0 }));
  $("phone-send").addEventListener("click", phoneCode);
  $("code-send").addEventListener("click", phoneVerify);
  $("phone-skip").addEventListener("click", () => { try { localStorage.setItem("tangkar.phone.skip", "1"); } catch (_) {} renderPhone(); });
  setSate(0);
  try { me = await whoami(); } catch (_) { return; }
  renderPhone();
  connect();
  setInterval(renderBoard, 250);
});
