# pneu — working notes for agents

Gmail-native mail client for Omarchy. README.md is the pitch, PLAN.md is the design
and build order; this file is the working contract. Read PLAN.md before touching code.

## Shape

- Gmail is the backend. lieer (`gmi`) syncs each account into a maildir, one
  notmuch database per account indexes it, a Go server on 127.0.0.1 renders it at
  `pneu.localhost`, and `pneu open` shows it as an `--app` window of the default
  browser. The app never talks to Google; `gmi` does, and only the server runs
  `gmi` unattended.
- The Omarchy integration lives here, generic: the shell plugin (`manifest.json`
  at the root, because `omarchy plugin add` clones the repo; `shell/BarWidget.qml`),
  and under `install/` the systemd unit, desktop entry, and theme-set hook. A
  user's own layout — scratchpads, keybindings, window rules — does not; the
  widget's `command` setting is how a layout takes over the click, and its
  `style` (Default | Minimal) how a sparse bar drops the idle mark. A layout
  that mounts the widget itself must hide the mount on the widget's `shown`,
  not `visible`: `visible` reads false under a hidden parent and never recovers.
  `omarchy plugin validate .` must pass, and it refuses a symlink anywhere in
  the repo. The widget draws its icon from `brand/pneu-mark.ttf` (a hinted
  glyph, like the bar's own icons), so a plugin packaged without `brand/`
  must carry the font. The shell doesn't reload a changed widget; run
  `omarchy-restart-shell`.
- One binary: `pneu` (or `pneu serve`) is the server; `pneu open` waits for it
  and launches or focuses the window through `omarchy-launch-or-focus-webapp`
  (pattern `pneu.localhost__open`, the class minus browser prefix and profile);
  `pneu gmi <account> <args>` is the manual lieer run (below);
  `pneu account add|auth|status` sets an account up (INSTALL.md step 5). All
  read `~/.config/pneu/config.json`; there are no built-in accounts, and
  `pneu account add` is what writes it.

## Code rules

- Go stdlib only: `net/http`, `html/template`, `os/exec`, hand-rolled SSE. No
  MIME library (`notmuch show` is the parser), no HTML sanitizer (DOMPurify in the
  browser). A dependency needs a reason written in the commit.
- Every notmuch and gmi call goes through the CLI (`--format=json`), never bindings.
  Select the account's database with `NOTMUCH_CONFIG`; the database that answered
  is the account. Retry notmuch writes on "already locked" — Xapian has one writer.
- Tag actions take the message-id list the view rendered, never `thread:X`.
- Frontend is server-rendered HTML plus vanilla JS. Vendor JS locally (DOMPurify
  included); no CDN, no bundler, no framework.
- Sanitized mail renders only inside a sandboxed `iframe srcdoc` with its own CSP.
  `allow-same-origin` is fine; `allow-scripts` never is — the sandbox is the
  script wall. Never inject message HTML into the app document. Any change to the sanitizer,
  sandbox flags, or CSP is verified against the hostile corpus before merge.
- Mutations are POST with Host and Origin checks. localhost is not a boundary:
  the default browser is also the daily browser, so every open tab can reach
  this port.
- Nothing writes to an account whose first pull hasn't completed
  (`Server.readOnly`): no tags, undo, mark-read or send. Its closing lastmod
  would swallow the write, and a resumed pull's label refresh would revert it.

## Environment

- go 1.27, notmuch 0.40 and lieer 1.6 are installed. Per-account notmuch configs
  live in `~/.config/pneu/<account>/notmuch-config`; INSTALL.md is the
  operator doc. Tests never touch those: `internal/testmail` builds fixtures in
  a temp dir from `testdata/`. Tests that need lieer's behaviour run
  `internal/testmail/cmd/stubgmi`, a lieer simulator (its output format, files,
  and token/stall/kill failures); `testdata/fakegmi` is the internal/gmi
  tests' contract fake. `scripts/rehearse` runs INSTALL.md itself in a
  sandbox (docs/rehearsal.md); keep it passing when INSTALL.md changes.
  `scripts/screenshot` captures the app on the fixture mail for the README
  and marketing (docs/screenshots.md); never screenshot a real inbox.
- The Arch notmuch is built with `retry_lock`, so a contended `notmuch tag`
  blocks rather than failing; `Tag()` bounds the wait with a deadline and
  returns `ErrLocked`. A long `gmi pull` stalls triage on that account for the
  deadline, by design.
- `notmuch show --format=raw --part=N` returns transfer-decoded bytes, but text
  parts keep their original charset; use the JSON's inline UTF-8 content for
  text and raw only for binary parts (`Part()` already does).
- A srcdoc iframe inherits the app page's CSP on top of its own meta CSP, and a
  load must pass both. The app page's policy (`web.AppCSP`, HTML responses only)
  is therefore exactly `script-src 'self'; object-src 'none'; base-uri 'none'`:
  a second script wall that names nothing the frame needs. Never add
  `default-src`, `img-src`, `style-src` or `font-src` to it.
- `/open?nonce=N` takes a single-use nonce from `~/.local/state/pneu/launch`,
  written by the server at startup and rotated on every successful open. The
  install token stays in the token file and the cookie; never log or print it.
  `pneu open` reads the nonce only after the server answers HTTP, which starts
  after the nonce is written, so it never reads a previous run's file.
- `~/.local/state/pneu/status.json` (0600, tmp+rename) is the bar widget's
  only input: unread inbox threads, five sender first names, per-account sync
  health, onboarding `state` and first-pull `progress`, `running`, `updated`. The server rewrites it at startup, after every
  sync or push, 500ms after a burst of tag writes, every 5 minutes, and with
  `running:false` on a clean stop; the widget calls it stale at 20 minutes.
  Never put the token, a nonce, or message content in it.
- `pneu gmi <account> <args>` takes the account's flock (the engine's lock,
  waiting up to 10 minutes), sets `NOTMUCH_CONFIG`, cds to the lieer dir, and
  execs gmi with the lock fd inherited, so gmi holds the lock for its own life.
- lieer semantics that matter (verified against upstream docs): tags `inbox`,
  `unread`, `flagged`, `trash`, `spam`, `sent`, `draft` map to labels. Archive is
  `-inbox`. Trash is `+trash -inbox` — only one of inbox/spam/trash may be set.
  `gmi send -t` reads RFC822 from stdin. Push refuses, without `-f`, any
  message whose Gmail historyId is above lieer's last pull ("remote has
  changed, will not update"), and the pull that follows overwrites the local
  tags. Pushing a message is such a change, so pneu's debounced push runs
  `gmi sync`, not `gmi push` (`gmi.OpPush`): its pull moves the historyId
  past our own change, and reading then trashing a message still sticks.
  Never `-f`: it would undo real remote changes.
- lieer pushes messages whose notmuch lastmod is above its stored lastmod. A
  full pull (not the usual partial one) ends by storing the database revision
  at its end, so a tag written during that sync is never pushed, and any
  pull sets messages with Gmail history to their remote labels, overwriting
  a write made while it ran. The engine records every tag write
  (`Engine.NoteWrite(account, changes, ids)`, called after each) and after a
  sync or push re-applies the ones made during it, or all it carried if lieer
  refused part of its push, then re-marks them with `+pneu-touch`
  `-pneu-touch` (a no-op re-apply doesn't bump lastmod) and pushes. Only
  pneu's own changes are replayed, never whole tag sets. Each repository
  ignores the tag (`gmi set --ignore-tags-local pneu-touch`, which
  `pneu account add` runs). lieer's refusal lines are logged.
- Onboarding (docs/onboarding.md): `gmi.FileState` reads an account's state
  from lieer's files (credentials are only stat'ed, never read). The engine
  runs the first pull itself (`firstPull`): `--resume` when a resume file
  exists, mail/tmp emptied first (unless lieer's own fcntl `.lock` is held),
  no timeout but a 10-minute watchdog that counts reads (`/proc/<pid>/io`)
  as well as output, SIGINT on shutdown, progress parsed from lieer's non-TTY
  output. Any run whose output says `invalid_grant` wraps `ErrReauth`; only
  then does the app offer Reconnect. Reconnect runs `gmi pull -t` first and
  stops there if the credentials work. Otherwise it runs
  `gmi auth -f -c <client JSON>` with `BROWSER=true` and hands the printed
  consent URL, which must be Google's, to the page. lieer's consent server is
  fixed at localhost:8080 and binds without SO_REUSEADDR, so a connection
  still in TIME_WAIT fails it for a minute; `CheckAuthPort` binds the same
  way. The test suite binds 8080 too: never run it while a real consent
  (an install's step 5, a Reconnect) is waiting. Every gmi run pneu starts
  sets `PYTHONUNBUFFERED=1`: its output is a pipe, and lines pneu acts on
  (the consent URL, progress) must arrive while gmi runs.
- `gmi send` prints its "receiving content" bar and then "message sent
  successfully: <gmail id>" only after the Gmail API accepted the message; a
  failure after that is the local copy's, and the message must not be resent
  (`gmi.Result.Accepted`).
- Theme contract: `install/pneu-theme` (installed as an Omarchy theme-set.d
  hook) writes only `color-scheme`,
  `--bg`, `--fg`, `--accent`, `--selection` to `~/.config/pneu/theme.css`;
  app.css derives muted/border/unread from bg/fg with `color-mix`. Don't add
  rungs to the hook or hardcode them in app.css.
  The hook also writes the launcher icon, the drawn mark alone in `--accent`
  (hicolor scalable only; launchers scale fixed sizes up on HiDPI and the
  pixel cut shows its blocks), from the copy of mark.svg INSTALL.md puts in
  `~/.local/share/pneu`: the hook runs as a copy and can't read brand/.
- An empty list shows pneu's mark (the `mark` template in base.html, the
  drawn cut of brand/mark.svg) as a faint watermark in the theme's colours.
- HTML bodies render in the app's colors unless the sanitized document
  declares a background or text color (inline style, `<style>`, `bgcolor`/
  `text`/`color`/`background` attributes), in which case the frame is the
  off-white sheet. The themed rule is built only from strictly validated
  `#rrggbb`/`light|dark` values read off our own stylesheet; the hostile
  corpus (cases 41–44) covers the detector.
- The databases are split per account because one shared database would merge
  any message with the same Message-ID (mail between the accounts, CCs to both)
  into one tag set and the two lieers would fight over it. Don't merge them.
