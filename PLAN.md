# Plan

## Shape

- **Gmail stays the backend.** Server-side filters, spam handling, and the phone app
  all keep working untouched. pneu is a view, not a migration.
- **Substrate:** [lieer](https://github.com/gauteh/lieer) syncs each account (Gmail
  API, two-way tag↔label) into a maildir; **one notmuch database per account**.
  notmuch is the entire data layer — the app has no database of its own. Two
  databases rather than one because notmuch keys tags by Message-ID: a message
  present in both accounts (mail between them, a CC to both) would share one tag
  set, and each account's pull rewrites tags to its own labels — a reply from the
  work address to the personal one lands back in the work inbox on the next push. The unified
  view is two queries merged by date; a cross-account thread shows twice, as it
  does on the phone.
- **Frontend:** a Go server bound to 127.0.0.1, opened as `http://pneu.localhost:PORT`
  in an `--app` window of the default browser via Omarchy's
  `omarchy-launch-or-focus-webapp`, which takes any Chromium-family default and
  falls back to chromium (Chromium resolves `*.localhost` to loopback without
  a hosts entry). Chromium is the HTML engine; that is the whole reason this is a
  web app — and the default browser's main process is already resident, so the
  window costs nearly nothing. A Qt or WebKitGTK reader would load a second
  browser engine (and a Qt one would join the `qt-app` scaling table; a chromium
  `--app` window inherits `browser-scale-sync` for free).
- **The Omarchy integration lives here; a user's layout doesn't.** The unit,
  desktop entry, theme-set hook, `pneu open`, and the bar widget (an Omarchy
  shell plugin, `manifest.json` at the repo root so `omarchy plugin add` can
  clone it) ship in this repo. Where the window sits — a scratchpad, a
  keybinding, window rules — is the user's dotfiles (the author's included),
  and the widget's click command is a setting so it can summon that layout.
  Sync is *not* glue; the server owns it (below).

## What matters

1. **Unified inbox.** `tag:inbox` across both databases, merged by date. It is the
   default and only primary view — like the phone's All Inboxes.
2. **Safe HTML.** Sanitize in the browser with vendored DOMPurify (`WHOLE_DOCUMENT`,
   so head `<style>` survives — marketing mail depends on it), then inject into an
   `iframe srcdoc` with `sandbox="allow-same-origin allow-popups
   allow-popups-to-escape-sandbox"` — same-origin so the parent can measure and
   auto-size the frame, popups so links open in a browser tab; **never**
   `allow-scripts`. A CSP `<meta>` as the first element of the frame's head:
   `default-src 'none'; img-src http://pneu.localhost:PORT data:; style-src
   'unsafe-inline'; font-src data:`. A srcdoc frame also inherits the app
   page's CSP, so the app page sets only `script-src 'self'; object-src 'none';
   base-uri 'none'` — nothing the frame's own policy has to grant. The sandbox
   is the script wall, DOMPurify is
   depth (it strips `<meta refresh>`, forms, `<base>`), CSP is what actually
   enforces remote-image blocking including CSS `url()`. Remote images load by
   default (`img-src` gains `https:`, never `http:`), minus receipt pixels: every
   URL on a read-receipt tool's host (Mailtrack, Superhuman, Yesware, HubSpot
   Sales…; never a bulk sender's host, which also serves the logos), and every
   remote `<img>` 3px or smaller or hidden by inline style. Spam and trash keep
   click-to-load — an open there confirms the address — which re-renders the
   srcdoc with the relaxed `img-src`, since a policy can't loosen in place. Links
   are rewritten to `target=_blank` after sanitizing (DOMPurify strips `target`).
   The frame gets a white background regardless of theme. `cid:` images served
   from a message-part endpoint.
3. **Triage as keystrokes.** Gmail bindings: `j`/`k` move, `e` archive (`-inbox`),
   `#` trash (`+trash -inbox` — lieer allows only one of inbox/spam/trash), `s` star
   (±flagged), `U` mark unread, `z` undo, `/` search. Opening a thread removes
   `unread`. Archive and trash act on the whole thread; star acts on the newest
   message, unstar clears every flagged message in the thread. Every action sends
   the explicit message-id list the view rendered, never `thread:X` — a reply that
   landed after render must not be swept up, and notmuch thread ids change on
   merge. Undo is the inverse tag op. The tag change lands in notmuch immediately;
   a debounced `gmi push` (a few seconds) follows so the phone agrees.
4. **Threads.** notmuch threads natively; conversation view, newest context first.
5. **Starred view.** The long-term shelf (bookings months out), one keystroke away.
6. **Search.** notmuch query syntax passed straight through. It's the engine, not
   a feature to build.
7. **Reply and compose, plain text.** `notmuch reply --format=json` builds From,
   In-Reply-To/References, recipients and the quote — with both addresses in
   `user.primary_email`/`user.other_email` it fills the recipients, but From is
   set by the server from the account whose database the thread came from —
   notmuch prefers whichever user address it finds in the original's headers,
   which is the wrong one for a message the other account sent. HTML-only mail is
   quoted from the sanitized DOM in the browser (the server has no HTML library).
   A reply that quotes goes out as multipart/alternative: the typed text, plus
   an HTML twin generated from it with the quote in Gmail's `gmail_quote` markup,
   since Gmail folds plain-text quotes only when it can match them to the
   original's text, which it often can't.
   The draft lives in localStorage so parking the scratchpad loses nothing.
   Address autocomplete from `notmuch address`, scoped to recipients of sent mail
   and cached. Send via `gmi send -t` in that account's directory; pull right
   after so the sent message appears in the thread.
8. **Attachments.** List, download, open — served from the same part endpoint.
9. **Open in Gmail.** One key per thread. lieer filenames carry the Gmail message
   id, which deep-links. This is the escape hatch that makes every exclusion below
   safe: RSVP, labels, rich compose, forwarding with attachments.
10. **Bar presence.** An Omarchy bar widget shipped as a shell plugin from this
    repo. Every notmuch change goes through the server (a sync or a tag write), so
    the server rewrites `~/.local/state/pneu/status.json` after each one — unread
    threads summed across the databases, recent senders, and each account's sync
    health — and the widget watches the file instead of polling. Sync health is
    the point as much as the count: an expired token otherwise kills sync
    silently, and the chip warns instead of showing a stale number.
11. **Theme.** CSS variables generated from the Omarchy theme's `colors.toml` by
    a theme-set hook shipped here; re-rendered on every theme-set. Mail bodies take the theme unless they declare their own colors.
12. **Sync.** A ticker goroutine in the server runs `gmi sync` per account every
    ~2 minutes, plus the debounced push after actions, and a sync when the
    window opens or regains focus. One process owns every
    write, so locking is an in-process mutex per account and SSE fires when a
    sync lands. Nothing else runs `gmi` unattended.

## What deliberately doesn't

- **Label management** beyond the four states (inbox / starred / trash / archive).
  Gmail filters keep doing any sorting server-side.
- **Rich-text composition.** Plain text with quoting.
- **Forward, and attachments on send.** Open in Gmail. Revisit after living on it.
- **Snooze, send-later, undo-send, templates, signatures, contacts.**
- **Calendar RSVP.** Open in Gmail.
- **Spam handling.** Gmail's job, server-side. `tag:spam` and `tag:trash` are in
  `search.exclude_tags` of both databases. Phishing that reaches the inbox gets
  trashed, not reported.
- **Push notifications.** Polling, plus a sync on open and focus; Gmail's push
  needs a Pub/Sub topic and a second credential, and pneu doesn't talk to Google.
- **Multi-user.** One person, one machine at a time.
- **Emptying trash.** Gmail's 30-day auto-expunge.

## Decisions

- **Go, stdlib-first.** `net/http`, `html/template`, `os/exec`, SSE by hand.
  Chosen because the server is 1–2k lines of plumbing that runs 24/7 as a user
  service on two machines: a single static binary, ~15MB resident, deploys by
  copy — no runtime, no bundler. The Ruby-shaped parts of the problem (models,
  DB, forms) don't exist here, and the UI half is HTML/CSS/JS regardless.
- **notmuch via CLI `--format=json`**, not C bindings. Stable interface,
  trivially debuggable — and `notmuch show --include-html` is the MIME parser,
  not just the index: GMime underneath decodes charsets and hands back the part
  tree with content-ids; `--format=raw --part=N` extracts attachments (verified
  transfer-decoded; text parts come from the JSON's inline UTF-8 instead, since
  raw keeps the original charset). The server
  ships no MIME library and no HTML-security library (DOMPurify runs in the
  browser).
- **Two databases, selected per call** with `NOTMUCH_CONFIG`. Account identity is
  which database answered. A `flock` wrapper still guards manual `gmi` runs
  against the server's own.
- **localhost is not a security boundary.** The default browser is also the daily one, so every
  open tab can reach the port; DNS rebinding can read bodies. Mutations are POST;
  every request checks exact `Host` and `Origin`; a per-install token in a
  `SameSite=Strict` cookie, set on the `--app` window's first load. Roughly thirty
  lines, and it ships in step 2, not later.
- **No frontend framework.** Server-rendered HTML plus a few hundred lines of
  vanilla vendored JS: keybindings, DOMPurify, click-to-load images, an SSE
  listener that refreshes the view when a sync lands.
- **Send via `gmi send`** (Gmail API), reading the RFC822 message from stdin, run
  in the originating account's directory so From/credentials match.

## Gotchas already known

- **OAuth token lifetime.** The Gmail scopes lieer needs are restricted. An
  External consent screen left in *Testing* expires refresh tokens after seven
  days and sync dies silently every week: for the personal account set the app
  *In production* and click through the unverified warning (allowed under the user
  cap). For a Workspace account mark the OAuth app **Internal** on a GCP
  project owned by that org — skips verification and the expiry. One client per
  account; Internal can't serve the gmail.com account. lieer ships shared
  credentials, so self-made clients are a choice, not a requirement.
- Initial `gmi pull` on a large archive takes hours and real disk, on each
  machine. The server never runs it under the sync timeout — learned the
  hard way: its two-minute sync tried the full pull under a ten-minute
  timeout, was killed every cycle, and held the flock against the manual
  pull the whole time. It now runs the first pull as its own operation: no
  timeout, a stall watchdog, interrupted on shutdown (docs/onboarding.md).
- **Xapian allows one writer per database.** The Arch notmuch is built with
  `retry_lock`, so a contended `notmuch tag` blocks until the writer releases;
  the server bounds that wait with a deadline (and retries on builds that fail
  fast). During the initial pull, triage on that account stalls to the deadline.
- **notmuch config per database:** `new.tags` empty (the default `unread;inbox`
  would mis-tag anything a stray `notmuch new` picks up), `new.ignore` for lieer's
  state files, `search.exclude_tags=spam;trash`, both addresses in `user.*`.
- **Part numbering is depth-first per message** and nests inside
  `message/rfc822`; build the cid→part map from the JSON tree. Verify that
  `--format=raw --part=N` returns transfer-decoded bytes.
- **Push ignores conflicting changes** without `-f`, and the next pull overwrites
  them. A keystroke and a phone action inside one sync window can revert; the
  view re-renders from notmuch after every sync so it never lies about it.
- Verify `gmi send` carries the Gmail threadId from `In-Reply-To`, or replies
  split into new threads on the phone.
- Chromium `--app` derives the window class from host and path, dropping the
  port and the query string. Measured: the launch URL `/open?nonce=N` gives
  `chrome-pneu.localhost__open-Default`, which is what `pneu open`'s focus
  pattern and any user scratchpad match; a nonce in the path would change the class on every launch. The
  collision to avoid is another `--app` window on the same host and path.
- Extensions share the profile: umber's content scripts can match `about:srcdoc`
  frames. Hence the fixed white frame background.
- Sanitizer round-trip: DOMPurify serializes and srcdoc reparses, which is exactly
  where mutation-XSS lives. It doesn't matter because the sandbox has no
  `allow-scripts` — but that is the wall, so never add it. Verify the assembly
  against a hostile corpus before trusting it.
- Each machine is its own Gmail client: own maildirs, databases, tokens, server,
  full pull. Never share the maildir or notmuch directories over the NAS,
  Syncthing, or dotfiles — lieer's state and Xapian both assume one local writer.
- Gmail's threading and notmuch's (References-header) threading can disagree at
  the margins. Accept notmuch's; don't chase parity.

## Later

- **First-run onboarding in the app.** Built 2026-09-25; design, decisions
  and what was decided while building in docs/onboarding.md. `pneu account add` / `pneu account auth` replace the
  hand-written account setup in INSTALL.md; the server runs the first pull
  itself, as a child, automatically, with the account read-only until it
  lands. No staged pull: lieer already fetches newest first and saves as it
  goes, so the inbox is usable minutes in, and progress (phase, exact total
  after listing, "complete back to <date>") comes from lieer's own output —
  the resume file only records that a pull is unfinished.
- **Image proxy.** At sync time the server fetches every remote image in new
  inbox mail and serves it from the app origin, as Apple Mail Privacy
  Protection does: every open looks like the same prefetch, so open tracking
  stops meaning anything, and image hosts see the server once instead of
  the browser on every open. The server resolves each host itself and refuses
  loopback and LAN addresses, which closes the DNS-rebinding gap the
  browser-side URL filter can't (mailframe.js), and `img-src` narrows back
  to the app origin.
- **Mailbox switcher.** Investigate. The merged stream (SPEC.md, Index views)
  suits the author, but other users may want their accounts kept apart —
  work and personal especially, where one list mixes contexts and a
  work-hours glance shouldn't surface personal mail. A switcher with "All"
  as one option, not a replacement for it. Open questions: whether the
  choice persists, whether the bar count and SSE refresh follow it, what
  `c` defaults to, and how an account marker returns to rows under "All".

## Build order

1. **Substrate** — OAuth clients, `gmi init` both accounts, full pull, one notmuch
   config per account. Zero code; gates everything.
2. **Read path** — merged list → thread view → sanitized HTML + part endpoint,
   with the Host/Origin/token checks from day one. The riskiest code, first.
3. **Triage and sync** — tag actions on message-id lists, mark read, undo, the
   sync ticker, debounced push, per-account mutex and write retry, SSE.
4. **Desktop glue** — scratchpad slot, bar widget, theme hook (first in the author's dotfiles;
   moved here, generalized, on 2026-09-25 — see Distribution).
5. **Reply and compose.**
6. **Live on it.** The Gmail PWAs stay docked until pneu earns the slot.

## Distribution

Nothing is published until it has been lived on as a stock install: the author's
machine runs exactly what INSTALL.md produces, with his dotfiles adding only
layout (a scratchpad slot pointed at `pneu open`).

- **Now: clone and follow INSTALL.md**, usually by handing it to a coding
  agent. The document is written for a reader that doesn't trust the repo: an
  Audit section states each security claim with the command that checks it,
  every step is a visible command (no install script, nothing piped from the
  network), human-only steps (the Google consent screen) are marked, and every
  installed file is a copy, so a later `git pull` changes nothing that runs
  until a deliberate reinstall. The bar widget is the one link: Omarchy loads
  plugins from its own directory.
- **Next: in-app onboarding** (Later, above; first pass in
  docs/onboarding.md). It decides how much of INSTALL.md's account setup the
  app absorbs; the first pull's progress and a usable inbox within minutes are
  the parts only the app can do well.
- **Then: publish.** A public repo that `omarchy plugin add` can clone for the
  widget, and an AUR package (`pneu`, depending on `notmuch` and `lieer`) that
  installs the binary, unit, desktop entry and hook to system paths. The
  package owns files, not accounts: OAuth and first pull stay per user. With a
  package, INSTALL.md collapses to its Audit section and account setup.
- **Undecided:** whether strangers can use lieer's shared OAuth client or
  must make their own (Testing-mode token expiry, the unverified-app user
  cap). Own client is the documented path until that's settled.
