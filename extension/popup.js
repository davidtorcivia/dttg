function $(id) { return document.getElementById(id); }

async function getConfig() {
  return browser.storage.local.get(["baseUrl", "token"]);
}

document.addEventListener("DOMContentLoaded", async () => {
  const cfg = await getConfig();
  if (!cfg.baseUrl || !cfg.token) {
    $("needs-config").hidden = false;
    $("form").hidden = true;
    return;
  }
  const tabs = await browser.tabs.query({ active: true, currentWindow: true });
  const tab = tabs[0];
  if (tab) {
    $("url").value = tab.url || "";
    $("title").value = tab.title || "";
  }
});

$("form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const cfg = await getConfig();
  const status = $("status");
  status.className = "";
  status.textContent = "Saving…";

  const payload = {
    url: $("url").value.trim(),
    title: $("title").value,
    note: $("note").value,
    category: $("category").value,
    tags: $("tags").value.split(",").map((s) => s.trim()).filter(Boolean),
    visibility: $("visibility").value,
  };

  try {
    const res = await fetch(cfg.baseUrl.replace(/\/+$/, "") + "/api/items", {
      method: "POST",
      headers: { "Authorization": "Bearer " + cfg.token, "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
    status.textContent = "Saved ✓";
    status.className = "ok";
    setTimeout(() => window.close(), 800);
  } catch (err) {
    status.textContent = "Failed: " + ((err && err.message) || err);
    status.className = "err";
  }
});

function openOptions(e) {
  if (e) e.preventDefault();
  browser.runtime.openOptionsPage();
}
$("open-options").addEventListener("click", openOptions);
$("open-options-2").addEventListener("click", openOptions);
