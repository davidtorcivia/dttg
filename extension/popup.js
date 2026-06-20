"use strict";

function $(id) { return document.getElementById(id); }

// Runs in the page (via scripting.executeScript) to pull the best title, a clean
// canonical URL, a description, and any current selection.
function pageMeta() {
  const meta = (key) => {
    const el = document.querySelector(
      'meta[property="' + key + '"], meta[name="' + key + '"]'
    );
    return (el && el.getAttribute("content")) || "";
  };
  const canonical = document.querySelector('link[rel="canonical"]');
  const sel = (window.getSelection && String(window.getSelection())) || "";
  return {
    title: meta("og:title") || document.title || "",
    url: meta("og:url") || (canonical && canonical.href) || location.href,
    description: meta("og:description") || meta("description") || "",
    selection: sel.trim(),
  };
}

async function activeTab() {
  const tabs = await browser.tabs.query({ active: true, currentWindow: true });
  return tabs[0] || null;
}

// Prefill from the active tab, scraping richer metadata when the page allows it.
async function prefillFrom(tab) {
  if (!tab) return;
  $("url").value = tab.url || "";
  $("title").value = tab.title || "";
  if (!tab.id) return;
  try {
    const out = await browser.scripting.executeScript({ target: { tabId: tab.id }, func: pageMeta });
    const m = out && out[0] && out[0].result;
    if (!m) return;
    if (m.url) $("url").value = m.url;
    if (m.title) $("title").value = m.title;
    if (m.selection) $("note").value = m.selection;
    else if (m.description) $("note").placeholder = m.description.slice(0, 140);
  } catch (_e) {
    /* privileged page (about:, PDF, store) — the tab url/title still stand */
  }
}

let taxonomy = { categories: [], tags: [] };

async function startForm(cfg) {
  $("form").hidden = false;

  $("visibility").value = cfg.lastVisibility === "private" ? "private" : "public";

  // Suggestion sources read `taxonomy` live, so they pick up the fresh fetch.
  makeCombo($("category"), () => taxonomy.categories, { multi: false });
  makeCombo($("tags"), () => taxonomy.tags, { multi: true });

  // Load suggestions without blocking the form — served instantly from cache
  // within the TTL, otherwise fetched. The combos read `taxonomy` live.
  getTaxonomy(cfg).then((t) => { taxonomy = t; });

  const tab = await activeTab();
  await prefillFrom(tab);
  $("title").focus();
  $("title").select();
}

async function init() {
  const cfg = await getConfig();
  if (!isConfigured(cfg)) {
    $("needs-config").hidden = false;
    return;
  }
  await startForm(cfg);
}

async function submit(e) {
  e.preventDefault();
  const btn = document.querySelector('#form button[type="submit"]');
  const status = $("status");
  status.className = "";
  status.textContent = "Saving…";
  btn.disabled = true;

  const visibility = $("visibility").value;
  const payload = {
    url: $("url").value.trim(),
    title: $("title").value,
    note: $("note").value,
    category: $("category").value.trim(),
    tags: $("tags").value.split(",").map((s) => s.trim()).filter(Boolean),
    visibility,
  };

  try {
    await saveItem(payload);
    await browser.storage.local.set({ lastVisibility: visibility });
    status.textContent = "Saved ✓";
    status.className = "ok";
    setTimeout(() => window.close(), 700);
  } catch (err) {
    status.textContent = (err && err.message) || String(err);
    status.className = "err";
    btn.disabled = false;
  }
}

function openOptions(e) {
  if (e) e.preventDefault();
  browser.runtime.openOptionsPage();
  window.close();
}

document.addEventListener("DOMContentLoaded", () => {
  init();

  $("form").addEventListener("submit", submit);
  $("open-options").addEventListener("click", openOptions);
  $("open-options-2").addEventListener("click", openOptions);

  // ⌘/Ctrl+Enter saves from anywhere; Esc closes the popup (combobox Esc is
  // handled first and stops propagation, so it only dismisses the dropdown).
  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter" && !$("form").hidden) {
      e.preventDefault();
      $("form").requestSubmit();
    } else if (e.key === "Escape") {
      window.close();
    }
  });
});
