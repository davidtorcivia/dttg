// Shared helpers for the popup and the background script. Loaded as a classic
// script in both contexts (manifest background.scripts + a <script> in the
// popup), so everything here lives on the global scope of each context.

"use strict";

const DNTTG_CONFIG_KEYS = ["baseUrl", "token", "lastVisibility"];
const TAXONOMY_TTL_MS = 5 * 60 * 1000;

async function getConfig() {
  return browser.storage.local.get(DNTTG_CONFIG_KEYS);
}

function apiBase(cfg) {
  return String((cfg && cfg.baseUrl) || "").replace(/\/+$/, "");
}

function isConfigured(cfg) {
  return !!(cfg && cfg.baseUrl && cfg.token);
}

// validUrl reports whether a string is a usable http(s) server URL.
function validUrl(baseUrl) {
  try {
    const u = new URL(baseUrl);
    return u.protocol === "http:" || u.protocol === "https:";
  } catch (_e) {
    return false;
  }
}

// apiFetch issues an authenticated, cross-origin request to the configured
// server. The server allows this via CORS; the bearer token is the only secret.
async function apiFetch(path, cfg, init) {
  init = init || {};
  const headers = Object.assign(
    { Authorization: "Bearer " + cfg.token },
    init.headers || {}
  );
  return fetch(apiBase(cfg) + path, Object.assign({}, init, { headers }));
}

// saveItem posts one item to the archive. Throws on any failure with a
// human-readable message. Returns the parsed {id, url} response.
async function saveItem(payload, cfg) {
  cfg = cfg || (await getConfig());
  if (!isConfigured(cfg)) {
    const err = new Error("Set the server URL and API token in the extension options.");
    err.code = "unconfigured";
    throw err;
  }
  let res;
  try {
    res = await apiFetch("/api/items", cfg, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
  } catch (_e) {
    throw new Error("Couldn't reach the server. Check the URL and your connection.");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (res.status === 401) throw new Error("Token rejected — re-mint it and update Options.");
    throw new Error((data && data.error) || "HTTP " + res.status);
  }
  return data;
}

// getTaxonomy returns {categories, tags}. Serves a cached copy instantly (within
// TTL) and otherwise revalidates from the server, falling back to any stale
// cache when offline. Pass {force:true} to bypass the TTL.
async function getTaxonomy(cfg, opts) {
  cfg = cfg || (await getConfig());
  const force = !!(opts && opts.force);
  const cached = await browser.storage.local.get(["taxonomy", "taxonomyAt"]);
  const fresh = cached.taxonomy && Date.now() - (cached.taxonomyAt || 0) < TAXONOMY_TTL_MS;
  if (fresh && !force) return cached.taxonomy;
  if (!isConfigured(cfg)) {
    return cached.taxonomy || { categories: [], tags: [] };
  }
  try {
    const res = await apiFetch("/api/taxonomy", cfg);
    if (res.ok) {
      const data = await res.json();
      const tax = {
        categories: Array.isArray(data.categories) ? data.categories : [],
        tags: Array.isArray(data.tags) ? data.tags : [],
      };
      await browser.storage.local.set({ taxonomy: tax, taxonomyAt: Date.now() });
      return tax;
    }
  } catch (_e) {
    /* offline or blocked — fall through to whatever we cached */
  }
  return cached.taxonomy || { categories: [], tags: [] };
}
