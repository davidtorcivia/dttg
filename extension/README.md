# DO NOT TOUCH THE GLASS — Firefox extension

Save images, links, selections, and whole pages to your archive, straight from
the browser. Works on desktop Firefox and Firefox for Android.

## What it does

- **Toolbar popup** — save the current tab with title, note, category, tags, and
  public/private, prefilled from the page. The page's canonical URL, `og:title`,
  description, and any current text selection are pulled in automatically.
- **Category + tag autocomplete** — suggestions are fetched from your archive
  (`GET /api/taxonomy`) and cached, so they appear instantly as you type. Arrow
  keys + Enter/Tab to accept; tags complete the token under the caret.
- **Right-click menu** — *Save image / link / selection / this page to the glass*.
- **Keyboard** — ⌘/Ctrl + Enter saves; Esc closes the popup.
- Posts to your server's `POST /api/items` with a bearer token; images and
  documents are downloaded, refined, and self-hosted server-side.

## Permissions & privacy

The extension requests **no host permissions** — it talks to your server purely
over CORS, authenticated by the bearer token (the server allows this only on the
token-gated `/api/*` routes). It declares no data collection. Permissions used:

- `activeTab` + `scripting` — read the current tab's metadata, only when you open
  the popup, only for that one tab.
- `contextMenus`, `notifications` — the right-click menu and save confirmations.
- `storage` — keep your server URL + token in this browser (never synced).

## Setup

1. On the server, mint a token:
   ```sh
   dnttg token firefox
   ```
2. Load the extension:
   - **Temporary (no signing):** open `about:debugging#/runtime/this-firefox` →
     *Load Temporary Add-on…* → pick `extension/manifest.json`.
   - **Packaged:** `npx web-ext build` in this folder, then install the `.zip`/`.xpi`
     (permanent install needs signing via [addons.mozilla.org](https://addons.mozilla.org),
     or Firefox Developer/Unbranded with `xpinstall.signatures.required=false`).
3. Open the extension's **Options**, set the **Server URL** and **API token**, and
   hit **Test connection** to confirm they work.

## Publishing to addons.mozilla.org

The package passes `web-ext lint` with zero errors/warnings. To submit:

```sh
npx web-ext lint          # should report 0 errors, 0 warnings
npx web-ext build         # writes web-ext-artifacts/*.zip
npx web-ext sign --channel=listed --api-key=… --api-secret=…   # or upload the zip on AMO
```

Once it's signed and listed, you install the signed build once and never touch
`about:debugging` again. Minimum Firefox: **142** (required by the manifest's
`data_collection_permissions` declaration — lower it in `manifest.json` if you
need to support older Firefox, at the cost of a lint warning).

## Android

Firefox for Android supports extensions. Either install the listed AMO version, or
use a [custom collection](https://extensionworkshop.com/documentation/develop/developing-extensions-for-firefox-for-android/).
Because the extension uses CORS instead of host-permission grants (Android can't
prompt for those), the same code runs unchanged on the phone.

## Bookmarklet (no install)

If you'd rather not install anything, the server's **Admin → Settings → Capture**
page has a draggable bookmarklet that opens the prefilled "new item" form for the
current page (uses your existing login instead of a token).
