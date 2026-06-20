# DO NOT TOUCH THE GLASS

A single-user, self-hosted visual archive — a personal, are.na-style board with an
awwwards-leaning editorial design. Drop in images, links, text notes, and embeds
from a Firefox extension or bookmarklet; tag them, file them in a category, mark
them public or private, and they appear live on the board.

- **Backend:** Go (stdlib `net/http`), server-rendered `html/template`
- **Storage:** SQLite (pure-Go `modernc.org/sqlite`, no cgo) + local media archive,
  with refined image variants mirrored to Cloudflare R2 and served from the CDN.
- **Deploy:** single static binary in Docker, behind a Cloudflare tunnel.

## Status

Milestone roadmap:

1. **M1 — Running foundation** ✅: DB, server, session auth, public board + detail
   rendered from SQLite, ported mockup design, seed data, self-hosted fonts,
   server-side weather.
2. **M2 — Ingest pipeline + R2 mirror** ✅: image processing (full/thumb variants,
   blur-up placeholder, dominant color), link OG scrape, YouTube/Vimeo oEmbed, R2
   `MirrorStore` (refined variants only) with local fallback + `reconcile` backfill.
3. **M3 — API + token auth + admin CRUD** ✅: `POST /api/items` (JSON + multipart,
   bearer token), admin dashboard, create/edit/delete, visibility, tags/category.
4. **M4 — Firefox extension + bookmarklet** ✅: context-menu + popup capture via
   API token; bookmarklet (prefilled admin form); Firefox-for-Android compatible.
5. **M5 — PWA share target + Docker/tunnel deploy** ✅: installable PWA with an
   Android share target; multi-stage Dockerfile (static binary → distroless),
   compose, `/share` endpoint.

**Also shipped:** search (header magnifier + `/search`), **PDF/document** uploads
(board tiles + in-browser viewer), embedded video on detail pages, self-hosted
Inter + server-side weather, an admin **settings** page with an injectable
analytics snippet, on-page **edit/delete** for admins, and rolling **R2 DB backups**.

## Quick start (local dev)

Requires Go 1.24+ (the module targets a recent toolchain for `crypto/pbkdf2`).

```sh
# from the repo root
DNTTG_DEV=1 go run ./cmd/dnttg set-password "choose-a-password"
DNTTG_DEV=1 go run ./cmd/dnttg seed
DNTTG_DEV=1 go run ./cmd/dnttg serve
# open http://localhost:8080  (log in at /login to see private items + admin)
```

Data lives in `./data` (SQLite + media), which is git-ignored.

## Commands

| Command | Purpose |
| --- | --- |
| `dnttg serve` | Run the HTTP server (default if no subcommand). |
| `dnttg migrate` | Apply migrations and exit. |
| `dnttg seed` | Download + refine demo content if the archive is empty. |
| `dnttg reconcile` | Push local-only refined variants up to R2 (backfill). |
| `dnttg backup` | Snapshot the DB to the private R2 backups bucket (+ prune old). |
| `dnttg reset-content` | Delete all items/media/tags/categories (keeps password + tokens). |
| `dnttg set-password <pw>` | Set/replace the admin login password. |
| `dnttg token [name]` | Mint an API token for the extension/bookmarklet (shown once). |

## Admin & API

- **Admin** is session-gated. Log in at `/login`; an admin bar then appears with
  **Admin** (`/admin` dashboard), **+ New** (`/admin/new` — paste a URL or upload),
  per-item **edit**/**delete**, and **logout**. Deleting an item also removes its
  media blobs (local + R2).
- **API:** `POST /api/items` with `Authorization: Bearer <token>`. Accepts JSON
  `{url|kind|title|note|category|tags[]|visibility}` or `multipart/form-data` with a
  `file` upload. Kind (image/link/text/embed) is auto-detected. Returns `{id, url}`.

```sh
curl -X POST https://<site>/api/items \
  -H "Authorization: Bearer $DNTTG_TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/x.jpg","category":"Texture","tags":["rust"]}'
```

## Deploy

Single static binary; templates, static assets, and migrations are all embedded.

```sh
cp .env.example .env            # fill in domains + R2 creds
docker compose build
docker compose run --rm dnttg set-password "your-password"
docker compose run --rm dnttg token firefox     # paste into the extension
docker compose up -d            # serves on :8080; data persists in the dnttg-data volume
```

Put it behind a Cloudflare tunnel and bind the R2 custom domain for media.
Deployment specifics (hostnames, R2 creds, tunnel) are intentionally kept in an
untracked `DEPLOY.local.md`, not this repo.

**PWA / Android:** open the site in the browser → *Add to Home Screen*. It then
appears in the Android **share sheet**, so you can share an image or link straight
into the archive.

## Configuration

All config is environment-driven; see [`.env.example`](.env.example). Media URLs
resolve to `DNTTG_MEDIA_BASE_URL` (the R2 custom domain) when set, otherwise the
local `/media` path — so the same DB works with or without R2.

> **Deployment / infrastructure specifics** (real domains, R2 bucket + custom
> domain, Cloudflare tunnel, server env, secrets) are intentionally **not** in
> this repo. They live in an untracked `DEPLOY.local.md` handed to the server-side
> agent. This repo is a clean, generic template; nothing here exposes secrets.

## Layout

```
cmd/dnttg/            entrypoint + subcommands (serve/seed/reconcile/token/…)
internal/config/      env-driven configuration
internal/store/       SQLite data layer + migrations (embedded)
internal/media/       storage backends: LocalStore, R2Store, MirrorStore
internal/ingest/      kind detection, fetch, image processing, OG scrape, oEmbed
internal/web/         HTTP server, auth, API, admin, templates + static (embedded)
internal/backup/      rolling SQLite snapshots to a private R2 bucket
extension/            Firefox (MV3) capture extension + bookmarklet
tools/genicons/       generates the PWA app icons
mockup/               original static design reference
```

## License

MIT © David Torcivia — see [LICENSE](LICENSE).
