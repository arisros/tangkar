"use strict";

document.addEventListener("DOMContentLoaded", async () => {
  const code = (new URLSearchParams(location.search).get("code") || "").toUpperCase().replace(/[^A-Z0-9-]/g, "");
  $("code").textContent = code || "----";
  try { await whoami(); } catch (_) { return; }
  $("go").disabled = !code;
  $("go").addEventListener("click", async () => {
    $("err").textContent = "";
    try {
      await api("POST", "/api/link", { code });
      $("go").hidden = true;
      $("done").hidden = false;
    } catch (e) { $("err").textContent = e.message; }
  });
});
