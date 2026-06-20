// Context-menu capture: image / link / selection / page -> POST /api/items.

const MENUS = [
  { id: "save-image", title: "Save image to the glass", contexts: ["image"] },
  { id: "save-link", title: "Save link to the glass", contexts: ["link"] },
  { id: "save-selection", title: "Save selection to the glass", contexts: ["selection"] },
  { id: "save-page", title: "Save this page to the glass", contexts: ["page"] },
];

browser.runtime.onInstalled.addListener(() => {
  browser.contextMenus.removeAll().then(() => {
    for (const m of MENUS) browser.contextMenus.create(m);
  });
});

browser.contextMenus.onClicked.addListener(async (info, tab) => {
  const title = tab && tab.title;
  let payload = null;
  switch (info.menuItemId) {
    case "save-image":
      // only that image, but linked back to the page it's on (the tweet, etc.)
      payload = { kind: "image", url: info.srcUrl, source: cleanSource(info.pageUrl || (tab && tab.url)), title };
      break;
    case "save-link":
      payload = { url: cleanSource(info.linkUrl), title: info.linkText || title };
      break;
    case "save-selection":
      // keep the source page as a link, with the highlighted text as the note
      payload = { url: cleanSource(info.pageUrl || (tab && tab.url)), note: info.selectionText, title };
      break;
    case "save-page":
      payload = { url: cleanSource(info.pageUrl || (tab && tab.url)), title };
      break;
  }
  if (payload) await saveItem(payload);
});

// For x.com / twitter.com expanded-image (and other deep) URLs, link back to the
// main tweet by dropping everything after /status/<id> (e.g. /photo/1).
function cleanSource(url) {
  if (!url) return url || "";
  const m = url.match(/^(https?:\/\/(?:x|twitter)\.com\/[^/]+\/status\/\d+)/i);
  return m ? m[1] : url;
}

async function saveItem(payload) {
  const cfg = await browser.storage.local.get(["baseUrl", "token"]);
  if (!cfg.baseUrl || !cfg.token) {
    notify("Not configured", "Set the server URL and API token in the extension options.");
    browser.runtime.openOptionsPage();
    return;
  }
  try {
    const res = await fetch(cfg.baseUrl.replace(/\/+$/, "") + "/api/items", {
      method: "POST",
      headers: { "Authorization": "Bearer " + cfg.token, "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
    notify("Saved to the glass", payload.title || payload.url || "");
  } catch (err) {
    notify("Save failed", String((err && err.message) || err));
  }
}

function notify(title, message) {
  try {
    browser.notifications.create({ type: "basic", title, message: message || "" });
  } catch (e) {
    /* notifications may be unavailable; ignore */
  }
}
