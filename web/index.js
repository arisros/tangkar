"use strict";

let me = null, data = null, offset = 0;

const label = { open: "buka", late: "late join", closed: "tutup", cancelled: "batal" };

function render() {
  if (!data) return;
  const t = Date.now() + offset;
  const list = $("list");
  if (!data.orders.length) {
    list.replaceChildren(el("p", { class: "lede" }, me.admin ? "Belum ada pesanan hari ini. Buka satu di atas." : "Belum ada pesanan hari ini. Tunggu link dari admin."));
    return;
  }
  list.replaceChildren(...data.orders.map(s => {
    const o = s.order, p = phase(o, t);
    const when = p === "open" ? "tutup " + hhmm(o.lock_at) + " · sisa " + left(Date.parse(o.lock_at) - t)
      : p === "late" ? "late join s.d. " + hhmm(o.late_until)
      : o.day_label;
    const failed = s.sends.some(x => x.status === "failed");
    return el("a", { class: "ticket", href: "/o/" + o.id },
      el("div", { class: "t1" }, el("span", {}, o.title), el("span", { class: p === "open" ? "ok" : p === "late" ? "warn" : "" }, label[p])),
      el("div", { class: "t2" }, when),
      el("div", { class: "t3" }, "DAGING " + s.totals.daging + " · CAMPUR " + s.totals.campur + " · SATE " + s.totals.sate,
        s.viewers ? " · " + s.viewers + " buka" : "", failed ? el("span", { class: "bad" }, " · WA gagal") : null));
  }));
}

async function create() {
  $("c-err").textContent = "";
  $("c-go").disabled = true;
  try {
    const r = await api("POST", "/api/orders", {
      title: $("c-title").value, lock: $("c-lock").value, late_minutes: parseInt($("c-late").value, 10) || 0,
    });
    $("made").hidden = false;
    $("made-url").textContent = r.url;
    $("made-open").href = "/o/" + r.order.id;
    $("made-copy").onclick = async () => { $("made-copy").textContent = (await copy(r.url)) ? "Tersalin ✓" : "Gagal salin"; };
    $("made-share").hidden = !navigator.share;
    $("made-share").onclick = () => navigator.share({ title: r.order.title, text: "Pesan " + r.order.title + ", tutup " + hhmm(r.order.lock_at), url: r.url }).catch(() => {});
  } catch (e) { $("c-err").textContent = e.message; }
  finally { $("c-go").disabled = false; }
}

document.addEventListener("DOMContentLoaded", async () => {
  try { me = await whoami(); } catch (_) { return; }
  if (me.admin) { $("create").hidden = false; $("c-go").addEventListener("click", create); }
  const es = new EventSource("/api/events");
  es.addEventListener("list", ev => { data = JSON.parse(ev.data); offset = data.now - Date.now(); render(); });
  es.onopen = () => { $("live-dot").className = "dot on"; $("live").textContent = "live"; };
  es.onerror = () => { $("live-dot").className = "dot"; $("live").textContent = "menyambung ulang"; };
  setInterval(render, 1000);
});
