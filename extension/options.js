function $(id) { return document.getElementById(id); }

document.addEventListener("DOMContentLoaded", async () => {
  const cfg = await browser.storage.local.get(["baseUrl", "token"]);
  $("baseUrl").value = cfg.baseUrl || "";
  $("token").value = cfg.token || "";
});

$("form").addEventListener("submit", async (e) => {
  e.preventDefault();
  await browser.storage.local.set({
    baseUrl: $("baseUrl").value.trim(),
    token: $("token").value.trim(),
  });
  const s = $("status");
  s.textContent = "Saved ✓";
  s.className = "ok";
  setTimeout(() => { s.textContent = ""; s.className = ""; }, 1500);
});
