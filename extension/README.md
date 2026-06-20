# DO NOT TOUCH THE GLASS — Firefox extension

Save images, links, selections, and whole pages to your archive, straight from
the browser. Works on desktop Firefox and Firefox for Android.

## What it does

- **Toolbar popup** — save the current tab with title, note, category, tags, and
  public/private, prefilled from the page.
- **Right-click menu** — *Save image / link / selection / this page to the glass*.
- Posts to your server's `POST /api/items` with a bearer token; images and
  documents are downloaded, refined, and self-hosted server-side.

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
3. Open the extension's **Options**, set:
   - **Server URL** — e.g. `https://your-archive.example.com` (or `http://localhost:8080` for dev)
   - **API token** — paste the token from step 1 (stored only in this browser)

## Android

Firefox for Android supports extensions. Either publish to AMO and install from
there, or use a [custom collection](https://extensionworkshop.com/documentation/develop/developing-extensions-for-firefox-for-android/)
in Firefox Nightly. The same code runs unchanged.

## Bookmarklet (no install)

If you'd rather not install anything, the server's **Admin → Settings → Capture**
page has a draggable bookmarklet that opens the prefilled "new item" form for the
current page (uses your existing login instead of a token).
