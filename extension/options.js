"use strict";

function $(id) { return document.getElementById(id); }

function setStatus(text, kind) {
  const s = $("status");
  s.textContent = text;
  s.className = kind || "";
}

document.addEventListener("DOMContentLoaded", async () => {
  const cfg = await getConfig();
  $("baseUrl").value = cfg.baseUrl || "";
  $("token").value = cfg.token || "";
});

$("form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const baseUrl = $("baseUrl").value.trim();
  const token = $("token").value.trim();

  if (!validUrl(baseUrl)) {
    setStatus("Enter a valid http(s) URL", "err");
    return;
  }

  await browser.storage.local.set({ baseUrl, token });
  await browser.storage.local.remove(["taxonomy", "taxonomyAt"]); // refetch for the new server

  setStatus("Saved ✓", "ok");
  setTimeout(() => setStatus(""), 2000);
});

$("test").addEventListener("click", async () => {
  const baseUrl = $("baseUrl").value.trim();
  const token = $("token").value.trim();
  if (!baseUrl || !token) {
    setStatus("Enter a server URL and token first", "err");
    return;
  }
  if (!validUrl(baseUrl)) {
    setStatus("Enter a valid http(s) URL", "err");
    return;
  }
  setStatus("Testing…");
  try {
    const res = await apiFetch("/api/taxonomy", { baseUrl, token });
    if (res.status === 401) { setStatus("Connected, but the token was rejected", "err"); return; }
    if (!res.ok) { setStatus("Server error: HTTP " + res.status, "err"); return; }
    const data = await res.json();
    const c = (data.categories || []).length;
    const t = (data.tags || []).length;
    setStatus("Connected ✓ — " + c + " categories, " + t + " tags", "ok");
  } catch (_e) {
    setStatus("Couldn't reach the server — check the URL", "err");
  }
});
