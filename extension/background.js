// Context-menu capture: image / link / selection / page -> POST /api/items.
// Shared config/fetch/saveItem helpers live in common.js (loaded first).

"use strict";

const MENUS = [
  { id: "save-image", title: "Save image to the glass", contexts: ["image"] },
  { id: "save-link", title: "Save link to the glass", contexts: ["link"] },
  { id: "save-selection", title: "Save selection to the glass", contexts: ["selection"] },
  { id: "save-page", title: "Save this page to the glass", contexts: ["page"] },
];

function buildMenus() {
  browser.contextMenus.removeAll().then(() => {
    for (const m of MENUS) browser.contextMenus.create(m);
  });
}

browser.runtime.onInstalled.addListener(buildMenus);
browser.runtime.onStartup.addListener(buildMenus);

browser.contextMenus.onClicked.addListener(async (info, tab) => {
  const title = tab && tab.title;
  let payload = null;
  switch (info.menuItemId) {
    case "save-image": {
      const src = info.srcUrl || "";
      if (/^(data:|blob:|moz-extension:|chrome:|about:|file:)/i.test(src)) {
        notify("Save failed", "Unsupported image URL (data/blob/privileged). Open the image on the web and try again.");
        return;
      }
      // only that image, but linked back to the page it's on (the tweet, etc.)
      payload = { kind: "image", url: src, source: cleanSource(info.pageUrl || (tab && tab.url)), title };
      break;
    }
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
  if (!payload) return;

  try {
    await saveItem(payload);
    notify("Saved to the glass", payload.title || payload.url || "");
  } catch (err) {
    notify("Save failed", (err && err.message) || String(err));
    if (err && err.code === "unconfigured") {
      browser.runtime.openOptionsPage();
    }
  }
});

// For x.com / twitter.com expanded-image (and other deep) URLs, link back to the
// main tweet by dropping everything after /status/<id> (e.g. /photo/1).
function cleanSource(url) {
  if (!url) return url || "";
  const m = url.match(/^(https?:\/\/(?:x|twitter)\.com\/[^/]+\/status\/\d+)/i);
  return m ? m[1] : url;
}

function notify(title, message) {
  if (!browser.notifications) return; // unavailable on some platforms (e.g. Android)
  browser.notifications
    .create({
      type: "basic",
      iconUrl: browser.runtime.getURL("icons/icon-96.png"),
      title,
      message: message || "",
    })
    .catch(() => {});
}
