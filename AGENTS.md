# pneu — working notes for agents

Gmail-native mail client for Omarchy. README.md is the pitch, PLAN.md is the design
and build order; this file is the working contract. Read PLAN.md before touching code.

## Shape

- Gmail is the backend. lieer (`gmi`) syncs each account into a maildir, one
  notmuch database per account indexes it, a Go server on 127.0.0.1 renders it at
  `pneu.localhost`, and `pneu open` shows it as an `--app` window of the default
  browser. Mail moves only through `gmi`, and only the server runs `gmi`
  unattended. pneu itself talks to Google only for the optional push sync
  (`internal/google`, docs/push.md): on a server where it's set up, the
  daemon and the push commands, three fixed API hosts, to learn *when* to
  sync, never mail.
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
  `omarchy-restart-shell`. The widget's decisions (status.json v1/v2,
  staleness, the link, the menu, the tooltip) are `shell/status.js`, plain
  JS the QML imports and `node --test shell/status.test.js` runs;
  `shell/test/` loads the real widget under Qt 6 against stand-ins for
  the shell (`QT_QPA_PLATFORM=offscreen /usr/lib/qt6/bin/qmltestrunner
  -import shell/test/imports -input shell/test`; `/usr/bin/qmltestrunner`
  is Qt 5 and fails silently). Every Text is `Text.PlainText`, every
  string from the file goes through `clean()`, enums are compared or
  checked as own properties (never `table[value]`), and the menu runs only
  `status.js`'s fixed argv through `Util.execArgv` (no shell), chosen by
  item id: nothing from status.json ever reaches a command (R1, R14).
- One binary: `pneu` (or `pneu serve`) is the server; `pneu open` waits for it,
  sends `launch` over the control socket (a sync on every account), and
  launches or focuses the window through `omarchy-launch-or-focus-webapp`
  (pattern `pneu.localhost__open`, the class minus browser prefix and profile);
  `pneu gmi <account> <args>` is the manual lieer run (below);
  `pneu account add|auth|status` sets an account up (INSTALL.md step 5),
  and on a client runs on the server over SSH (below);
  `pneu push init` and `pneu account push <name> [--reconsent|--off]` set
  up push sync (INSTALL.md "Instant mail"; below), the same way;
  `pneu peer add --stdin|list|remove` pairs other machines' clients (below);
  `pneu client pair <ssh-target>|unpair` makes this machine one (below);
  `pneu agent [-print]` and `pneu reset-window` are the bar menu's Fix with
  agent and Reset window data (docs/client.md "As built: step 6"; the
  reset closes only pneu's app windows, says to quit the browser for any
  other pneu tab, and stops when a browser window's title shows pneu).
  `pneu update [--check] [--yes]` updates this machine from its own
  checkout and the remote and branch `pneu source set` recorded (either
  mode; docs/client.md "As built: step 7"), and `pneu version` prints the
  build (`pneu <revision>`), which update's smoke check reads.
  Commands printed for the user quote the SSH target (`config.ShellWord`),
  and printed ssh commands carry the safe options (`config.SSHHint`). All
  read `~/.config/pneu/config.json`; there are no built-in accounts, and
  `pneu account add` is what writes it. A config with `server` (a client's,
  docs/client.md) makes `pneu serve` the client daemon; with `accounts` or
  `peer` as well it is refused.

## Code rules

- Go stdlib only: `net/http`, `html/template`, `os/exec`, hand-rolled SSE. No
  MIME library (`notmuch show` is the parser), no HTML sanitizer (DOMPurify in the
  browser). A dependency needs a reason written in the commit.
- Every notmuch and gmi call goes through the CLI (`--format=json`), never bindings.
  Select the account's database with `NOTMUCH_CONFIG`; the database that answered
  is the account. Retry notmuch writes on "already locked" — Xapian has one writer.
- Tag actions take the message-id list the view rendered, never `thread:X`.
- Frontend is server-rendered HTML plus vanilla JS. Vendor JS locally (DOMPurify
  and marked included, each with a `*_VERSION` provenance file); no CDN, no
  bundler, no framework.
- Sanitized mail renders only inside a sandboxed `iframe srcdoc` with its own CSP.
  `allow-same-origin` is fine; `allow-scripts` never is — the sandbox is the
  script wall. Never inject message HTML into the app document. Any change to the sanitizer,
  sandbox flags, or CSP is verified against the hostile corpus before merge.
- Attachments (docs: SPEC.md, Thread page): the server picks each one's viewer
  kind (`attach.go`, `data-view`) from the same effective type `/part` serves,
  so the two agree. HTML attachments and Markdown render through
  `renderMailFrame` like a body; Markdown is parsed in the browser by vendored
  marked (Go's stdlib has no Markdown, and this way its output goes straight
  to DOMPurify). Everything else the viewer builds with textContent. `/part` never
  serves an active type: SVG is `image/svg+xml` only to `Sec-Fetch-Dest:
  image` (an `<img>` runs no script), text/plain otherwise. A PDF is the one
  unsandboxed frame, because Chromium's viewer refuses a sandbox CSP; it gets
  `frame-ancestors 'self'` and `X-Frame-Options: SAMEORIGIN` in place of the
  global DENY (`part-pdf`, only on `/part` with a parsed `application/pdf`),
  and nothing else is frameable.
- Security headers come only from the policy classes (`policy.go`: `app`,
  `compose`, `data`, `part-sandbox`, `part-pdf`, `static-svg`), applied at
  the final header write (1xx writes are swallowed) and named in
  `Pneu-Policy` (and the process in `Pneu-Instance`, pneu update's
  readiness check); SVG is never `data`; a handler picks one with `usePolicy`
  or gets `app` for HTML and `data` otherwise. Never set a security header
  (`Clear-Site-Data` included: only `Auth.ArmClearSite` does) in a handler. Routes come only from the table (`routes.go` `Routes`): a
  new route is an entry with its classes and their media types, its
  disposition, Sec-Fetch-Dest, redirect and cache rules, or it doesn't
  exist. `Checker.Check` is the contract; every response in the web tests
  goes through it and through `Checker.Admit`, which the client proxy runs
  on every upstream answer. A route's `Wait` is the client's
  response-header timeout on it (10s unless the handler is slow on
  purpose, as `/send` is).
- Mutations are POST with Host and Origin checks. localhost is not a boundary:
  the default browser is also the daily browser, so every open tab can reach
  this port.
- A write that changes what a list or thread shows bumps the view
  generation once it has written anything (`viewChanged`, view.go; SPEC.md
  "Other windows"), naming the threads it wrote as the database says, not
  as the page does. `X-Pneu-Window` (`from`) is a hint for skipping one
  page's own refresh; nothing may authorize or trust anything on it.
  `/events` subscribes and snapshots `hello` under the locks that order
  `view`, `account` and `status`: keep new state events behind the same
  locks. A client daemon does the same for its pages under its one mutex.
- Nothing writes to an account whose first pull hasn't completed
  (`Server.readOnly`): no tags, undo, mark-read or send. Its closing lastmod
  would swallow the write, and a resumed pull's label refresh would revert it.

- Agent callouts (`internal/callout`): `pneu agent` asks the daemon for
  its `situation` and hands `omarchy-agent-prompt` one fixed template per
  situation, as one argv element. The only values in it are local: the
  situation code, this binary's revision, the config's SSH target, the
  server's revision only as 40 hex (else "unknown"), and on a server its
  own account names (a client's prompt only counts the failing accounts:
  their names are the server's). Never a
  server's error text, its name for itself, or anything SSH printed; the
  prompt says what it reads from the server is data. Golden prompts in
  `internal/callout/testdata` (`go test ./internal/callout -update`, then
  read the diff).
- Versions (`internal/update`, docs/client.md "As built: step 7"): two
  builds are the same only at one revision with neither modified; which is
  older is only `git merge-base --is-ancestor` in this machine's recorded
  checkout after a fetch, run by `pneu update` (never by the daemon, never
  from a timestamp: `vcs.time` and commit dates are never read), cached
  per revision pair in `skew.json` for the daemon to read. Anything
  undecided is `different`. `status.json`'s `server.update` and the page's
  `link.update` carry only the enum and 40-hex-or-`unknown` revisions.
  `pneu update` takes code only from the recorded remote (refusing one
  whose URL changed), fast-forward only, builds beside the live binary
  before touching anything, records `update.json` before deploying, and
  rolls back binary, checkout (only if still clean at the new revision)
  and service as one transaction; readiness is the control socket's
  `status` at the new revision with a new instance plus Auth's cookieless
  403 on loopback carrying `Pneu-Policy` and the same `Pneu-Instance`
  (U5: every policy-writer answer names its process; `status` says
  `listening` only once HTTP is bound). Before the yes only git runs, on
  the recorded checkout, and every read-only git call is
  `--no-lazy-fetch` with `GIT_NO_LAZY_FETCH=1` (a partial clone must
  never fetch a revision the server named); the live binary's build is
  `debug/buildinfo`, never a Go subprocess. Every child inherits the
  update lock (fd 3) so a killed updater's build still holds it; the
  staged build is `.pneu.new.<tx>`, fsynced, re-hashed before the rename;
  directory syncs and the record's removal are checked; the target is
  always held to full readiness (a legacy-style answer is only accepted
  for the old build in a rollback, when update.json's `oldLegacy` says
  so); an unreadable baseline is never "no baseline"; branch and HEAD are
  read again (one snapshot) at every exit that speaks of the checkout,
  shortcuts and `--check` included, never assumed; mutating git runs
  with hooks, auto gc, auto maintenance and fsmonitor off; the network
  fetch and read-only git don't inherit the lock (a credential daemon
  would keep it); `update.json` and
  `skew.json` are parsed by token (docs/client.md "As built: step 7,
  review fixes"). Tests: temp repos, real Go builds of a stub module
  through a wrapper, a fake systemctl, a control socket and HTTP
  listener playing the daemon; never the real ones.

## Environment

- go 1.27, notmuch 0.40 and lieer 1.6 are installed. Per-account notmuch configs
  live in `~/.config/pneu/<account>/notmuch-config`; INSTALL.md is the
  operator doc. Tests never touch those: `internal/testmail` builds fixtures in
  a temp dir from `testdata/`. Tests that need lieer's behaviour run
  `internal/testmail/cmd/stubgmi`, a lieer simulator (its output format, files,
  and token/stall/kill failures); `testdata/fakegmi` is the internal/gmi
  tests' contract fake. `scripts/rehearse` runs INSTALL.md itself in a
  sandbox (docs/rehearsal.md): its numbered steps, the server's install;
  unnumbered sections (Instant mail, Let other machines in, the Client
  path) aren't rehearsed, but Uninstall is: a command there must succeed
  on an install without them. Keep it passing when INSTALL.md changes.
  `scripts/screenshot` captures the app on the fixture mail for the README
  and marketing (docs/screenshots.md); never screenshot a real inbox.
  `scripts/ctaeval <account> [N]` is the one tool that reads real mail on
  purpose: it runs o's button heuristic over the account's recent HTML
  bodies read-only, in Chromium with all network blocked, bodies handed over
  the DevTools pipe (never HTTP), and never prints message content beyond
  link labels and hosts, nor an error's own words (docs/actions.md,
  "Evaluating it"). Keep it that way; test it on the fixture only.
- The Arch notmuch is built with `retry_lock`, so a contended `notmuch tag`
  blocks rather than failing; `Tag()` bounds the wait with a deadline and
  returns `ErrLocked`. A long `gmi pull` stalls triage on that account for the
  deadline, by design.
- `notmuch show --format=raw --part=N` returns transfer-decoded bytes, but text
  parts keep their original charset; use the JSON's inline UTF-8 content for
  text and raw only for binary parts (`Part()` already does).
- A srcdoc iframe inherits the app page's CSP on top of its own meta CSP, and a
  load must pass both. The app page's policy (`web.AppCSP`, the `app` and
  `compose` classes) is therefore exactly `script-src 'self'; object-src
  'none'; base-uri 'none'; worker-src 'none'`: a second script wall that
  names nothing the frame needs, and no service worker (one would outlive
  the page and any fix). Never add `default-src`, `img-src`, `style-src` or
  `font-src` to it.
- `/open?nonce=N` takes a single-use nonce from `~/.local/state/pneu/launch`,
  written by the server at startup and rotated on every successful open. The
  install token stays in the token file and the cookie; never log or print it.
  `pneu open` reads the nonce only after the server answers HTTP, which starts
  after the nonce is written, so it never reads a previous run's file.
- The control socket, `$XDG_RUNTIME_DIR/pneu/control` (`internal/control`,
  docs/client.md), is the only way a process outside the browser talks to
  the server, and no page can reach it: one command line per connection
  from a fixed set (`launch`, `status` (build, instance, `listening` once
  loopback HTTP is up), `reset-window` (arms
  `Clear-Site-Data: "cache", "storage"` for the next authenticated
  same-origin top-level navigation, once, either mode), `unlink` (a client's daemon only:
  it drops the link and acks once its connections are closed; `pneu client
  unpair` sends it before deleting anything), `situation` (the daemon's
  view for `pneu agent`: mode, a client's link reason code, the server's
  revision as 40 hex, failing account names, the skew state; JSON on one
  line, parsed by token: each key once, nothing after, every field
  bounded), `update-checked` (a client's daemon only: reread skew.json),
  `peers-reload <gen> <hash>`
  (a client's daemon refuses it), `push-reload <gen> <hash>` and
  `push-state <account>` (push sync, below; a client's daemon answers
  push off), whose only arguments are a decimal generation and 64 hex
  digits, or an account name, validated before any handler runs), both ends checking `SO_PEERCRED`
  for our uid, the directory 0700 and ours (`Lstat`, no symlink) or the
  server runs without it. `/open` starts no sync. A throwaway server
  (screenshots, the fixture server) sets its own `XDG_RUNTIME_DIR`, or a
  real `pneu open` could launch it.
- The peer listener (`internal/peer`, docs/client.md "The link"; only with
  `"peer": {"port": N}` in the config, and only with the control socket up)
  binds this node's own tailnet addresses from tailscaled's LocalAPI
  (`internal/tailscale`: the fixed root-owned socket, never a path from the
  environment, 2s per call), reconciled every 10s, never a wildcard. TLS
  1.3, HTTP/2 only, `RequireAnyClientCert`, no session tickets; the pin (SPKI
  SHA-256 in the live generation of `peers.json`) and the whois predicate are
  checked in `VerifyConnection`, and a connection is registered only after
  its full handshake (Go checks the client's CertificateVerify after
  VerifyConnection), under the lock a reload takes. Each connection holds a
  60s whois lease, renewed by a 10s sweep whatever it carries, and is closed
  with all its streams when the lease lapses or whois refuses. Every request
  goes through `web.PeerHandler`: the peer is looked up afresh
  (`Identify`), Host must be a listener address, there's no cookie, Origin
  or nonce, `Page.Origin` is the peer record's (`Server.origin`, never a
  header), and every answer carries `Pneu-Protocol`. Routes come from the
  table: `Local` ones aren't served there unless `Upstream` (`/events`);
  `PeerOnly` (`/peer/hello`) exists only there. Logs name peers and nodes,
  never certificates or pins.
- The client daemon (`internal/client`, `internal/link`; docs/client.md "As
  built: step 5a") serves pneu.localhost behind the same `Auth`, answers
  `/open`, `/theme.css` and `/events` itself, and proxies only routes in
  the table (`web.ClientRoutes`; anything else is a local 404, and a
  `Service-Worker` request a 404). Up go no cookie, Origin, Referer,
  Authorization, forwarding or hop-by-hop headers, and no validators on
  `/static/`. Down comes only what `Admit` rebuilds after Check (one
  Content-Type, Content-Length, part ranges, a rebuilt disposition, a
  checked Location), installed on the policy writer at the first final
  status: 1xx swallowed, trailers dropped, never Set-Cookie, ETag or the
  server's cache or security headers. No body ever goes out without a
  Content-Type (net/http would sniff one, nosniff or not): only 204, 304
  and 301/302/303/307/308 may be untyped, and they carry no body, on both
  the server's writer and the proxy's. A local failure says `Pneu-Link:
  not-sent` only when no connection was ever handed to the request
  (GotConn), else `unknown`; a compose form's own POST `/send` gets the
  client's send page instead (compose class, never reloads itself). The link dials only the address its own
  tailscaled gives for the pinned StableID after a whois passes, over one
  transport per up period whose connections all close when it goes down or
  its 60s whois lease lapses; `link.PinnedTLS` is the only place
  `InsecureSkipVerify` may appear. Its state is a local reason code, never
  server text. Credentials: `peer/client.pem` and `peer/pin.json` in the
  state dir (0700 dir, 0600 files, checked on every load), never
  config.json. The client daemon holds an flock on `peer/daemon.lock`
  (never replaced) for its life and won't run without it or without its
  control socket, both taken before it loads credentials; `pneu client
  unpair` sends `unlink` and, with no daemon answering, deletes only under
  that lock. `peer/pair.lock` makes pair (read, then write), unpair (read,
  ask, then delete) and a daemon's start (lock, then load credentials) one
  transaction each, so none interleaves with another. A tracked
  connection's close and its removal are one `once.Do`. A session's close waits for dials
  in flight as well as closing its connections. `Link.Unpair` returns only once every session the link owns
  (pending, live, going down) has finished closing. Pairing runs one fixed `ssh -T -o … -- <target>
  'PATH="$HOME/.local/bin:$PATH" exec pneu peer add --stdin'` (a
  non-interactive SSH session may not have ~/.local/bin on PATH; exit 127
  says so), the request on stdin and the answer parsed strictly
  (`peer.ParseAddResult`).
  Its live side (docs/client.md "As built: step 5b"): exactly one upstream
  `/events`, whose first event must be `hello` and after which only
  `syncing`, `sync`, `account`, `auth`, `status` and `view` pass, each
  ≤ 64 KiB, decoded into its `web` type, held to shape (accounts in the
  link hello's set, enums, plain bounded text, a view's gen above the
  last) and re-encoded; never raw bytes, never `link`/`theme`/`hello`
  from upstream; every byte and block charged to a token-bucket budget
  (`client.Budget`) before parsing, over it the stream closes and backs
  off, reset only after 60s healthy. 60s of silence calls down the
  session that carried the stream (`Link.Stalled(id)`), never a later
  one. Account names are `config.ValidName` everywhere, exactly
  `^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`: refused (never cleaned) at the
  server's config, `pneu account add`, the link's hello and the handshake;
  a suggested command shows `<account>` for anything else.
  Sleep (`internal/wake`, docs/client.md "Waking from sleep"): every Go
  timer stops in suspend, so the link, the client's status.json and a
  server's status.json and sync watch for a wall-clock jump instead;
  on one the session goes and the link says `starting`, and stays so
  through transient failures for `link.Waking` (the page shows a
  welcome-back progress page off `State.Asleep`). A request that
  finds the link `starting` waits up to `SettleWait` for it; a safe one
  (GET/HEAD, no body) that dies on a live session is retried once on a
  fresh one (`Link.Reconnect`), a mutation never; a failed page
  navigation always gets the error page.
  Pages get the daemon's own `hello` (state as last known, plus `link`),
  `link` on every change, its own desk's `theme`. The server's name shown
  anywhere is the SSH target, never hello's. status.json there is version
  2 (`client.StatusV2`: this daemon's `updated`/`running`, the last valid
  status's counts within limits, `server` in local codes), at most one
  write a second. A page navigation while the link isn't up is the
  binary's own error page (`/client/static/`); `/client/link` (read) and
  `/client/retry` (no input) are the only other `/client/` routes, all
  `ClientOnly` entries in the route table.
- Server work from a client (`internal/remote`, `cmd/pneu/accountremote.go`,
  `relay.go`, `accountstdin.go`; docs/client.md "As built: step 8" and its
  Y1–Y3): `pneu account add|auth|status` on a client runs `ssh -T
  <config.SafeSSHOptions> -- <target> 'PATH="$HOME/.local/bin:$PATH" exec
  pneu account <verb> --stdin'`, the command a fixed string per
  `remote.Verb` (never built from input), the parameters one JSON object
  as stdin's first line (16 KiB; exact keys, each once, strings and
  booleans, all required; `remote.Parse*`). The server answers with
  line-delimited events (`progress`, `waiting`, `consent-url`, `result`,
  `error`; ≤ 8 KiB a line, exact keys per kind, text plain at 500 runes, a
  closing `result`/`error` and nothing after; `remote.ParseEvent`); its
  stderr reaches the terminal only as plain prefixed lines. **No pneu ssh
  ever forwards anything**: every one, consent included, has
  `ClearAllForwardings=yes` (a real-ssh `-G` test holds it against a
  config adding forwards, `Match command` included); never add `-L`, `-R`,
  `-D` or `ClearAllForwardings=no`. auth's consent comes back through the
  relay: the client binds 127.0.0.1:8080 and [::1]:8080 itself, takes one
  `GET /` whose query is Google's callback keys only with this consent's
  state (`remote.ParseCallback`), answers fixed text (never reflecting the
  request), and sends `{"callback":…}` as one more stdin line; the server
  replays it to lieer on 127.0.0.1:8080. The consent URL is opened only if
  `gmi.ValidConsentURL` passes, the one rule every consent URL meets. The
  remote command's context (heartbeat, stdin's end, signals) covers the
  lock wait and every gmi it runs; each gmi pneu account runs outside a
  terminal is a process group of its own (`procgroup.go`): SIGINT, grace,
  SIGKILL to the group, and the lock is released only once the group is
  gone. Consent is bounded at 10 minutes and the token check at 2; on the
  client, a closing event closes the relay and stdin at once (ssh killed
  5s later if it lingers), and the relay has a 3-minute bound for the
  consent URL and 10 minutes from it, with time, size and connection
  limits on its listener. `pneu gmi` on a
  client refuses. Every ssh command pneu prints is `config.SSHHint` (the
  same safe options), or a local `pneu account …` where that exists. Fix
  with agent has a `reauth` situation (`control.Situation.Reauth`, a
  count), and a client's page shows `pneu account auth <name>` instead of
  Reconnect (the server answers that 409 `reauth-on-server`).
- `peers.json` (`$XDG_STATE_HOME/pneu`, 0600, O_NOFOLLOW, a generation and
  a hash over its content) is written only by `pneu peer add|remove` under
  an flock on `peers.lock`, a file never replaced (N8), with temp + fsync +
  rename + fsync dir. They hold the lock until the daemon acknowledges
  (`control.ReloadPeers`, 5s) naming generation and hash; a removal is
  acknowledged only once that peer's connections are closed and their
  handlers returned, including connections that closed before it (a
  connection leaves the registry only when finished). No daemon: "applies at next start". No ack: "pending",
  never "done". The server's key pair is `peer/server.pem` (dir exactly
  0700, file 0600, both checked on every load).
- Push sync (`internal/google`, `internal/push`, `cmd/pneu/push.go`;
  docs/push.md, its "As built" sections): server only, optional per
  account; the 30s poll is untouched and push only adds syncs. A Gmail
  watch per mailbox (`INBOX` changes only) posts to topic `pneu-<account>` in the user's **push
  project**, a GCP project of its own (never lieer's) with its own
  Desktop client; the daemon keeps two `:pull`s outstanding on
  subscription `pneu-<account>` with the owner's token (`pubsub`,
  `openid email`; `sub` pinned at init), and each mailbox's token is only
  `gmail.metadata` (getProfile, watch, stop). A message is only a nudge:
  `Engine.Nudge(account)` then ack; its body is never decoded, so a
  forged one can only cause a sync. One server per push project
  (subscriptions carry a `pneu-install` label; another's is refused).
- Push state is `$XDG_STATE_HOME/pneu/push/` (dir exactly 0700, `Lstat`;
  files 0600, `O_NOFOLLOW`, size-capped, parsed by token): `state.json`
  (generation, project, install, client, owner, accounts `on` |
  `off-pending`, refresh tokens) and `client.json`, written only by the
  CLI under `push.lock` (never replaced; temp + fsync + rename + fsync
  dir, generation+1, SHA-256 of the bytes), never config.json. The daemon
  only reads them, holds `daemon.lock` for life, and applies a generation
  on `push-reload <gen> <hash>` (`ok` | `stale` | `mismatch`; workers
  cancelled and joined before `ok`, no network in the handler);
  `push-state` reports instance, generation and health. The CLI decides
  "no daemon" only by taking `daemon.lock`; no ack is "pending", never
  "done". Consent and network calls run outside `push.lock`, and every
  commit re-reads under it and refuses what changed meanwhile; the one
  exception is `--off`: off-pending, ack, refresh and `users.stop`,
  removal, holding the lock throughout, since released around the stop
  a concurrent `account push` could start a watch the stop then ends
  (`users.stop` ends every watch on the mailbox). Topic, subscription
  and grants stay.
- The Google boundary is `internal/google` alone: one `http.Client`,
  three constant hosts (`oauth2`, `gmail`, `pubsub.googleapis.com`; tests
  swap a base through an unexported hook, never config or env), no
  redirects, no environment proxy, bodies capped at 64 KiB into fixed
  structs, every token answer's scope set checked exactly. Failures are
  `*google.Error{Op, Code}` in a closed vocabulary; nothing Google wrote
  reaches a log, a terminal, a remote event, a page or status.json.
  `cmd/pneu/pushwords.go` is the only place a failure becomes words, and
  the daemon logs only op and code. Push's health (`push:` in the account
  view and status.json, closed enums) never feeds `sick`.
- Push consent: pneu builds the URL (PKCE, state, the owner's nonce),
  which must pass `gmi.ValidConsentURL`. On the server it binds
  127.0.0.1:8080 and [::1]:8080 lieer's way (no SO_REUSEADDR) and holds
  them until the callback; from a client the relay forwards the callback
  line and the server checks `state` against its own consent in constant
  time and exchanges only the code (remote verbs `push`, `push-off`,
  `push-init`; the push client JSON travels as its three fields). A
  working stored grant is reused unless `--reconsent`. An owner consent
  must be the pinned `sub` (a new owner only with `push init
  --replace`, every account off); a mailbox consent must be the
  configured address by `getProfile`, and no two accounts one address. pneu never revokes a grant;
  it points to the permissions page with the owner caveat (an owner that
  is also a pushed mailbox loses both grants to one revocation).
  Tests use `internal/google/googletest` (a fake of every call, failure
  injection, a fake clock), never real Google.
- `~/.local/state/pneu/status.json` (0600, tmp+rename) is the bar widget's
  only input: unread inbox threads, five sender first names, per-account sync
  health, onboarding `state` and first-pull `progress`, `running`, `updated`. The server rewrites it at startup, after every
  sync or push, 500ms after a burst of tag writes, every 5 minutes, and with
  `running:false` on a clean stop; the widget calls it stale at 20 minutes.
  Each rewrite is also SSE `status` (the same doc), and `/events`' hello
  carries the last one: a client's status.json (version 2) is built from it.
  Never put the token, a nonce, or message content in it. The widget
  accepts versions 1 and 2.
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
  (the consent URL, progress) must arrive while gmi runs. It also sets
  `PYTHONIOENCODING=utf-8` and `PYTHONUTF8=1` over any inherited values
  (`gmi.PythonEnv`), so the ASCII markers pneu matches read as written.
- `gmi send` prints its "receiving content" bar and then "message sent
  successfully: <gmail id>" only after the Gmail API accepted the message; a
  failure after that is the local copy's, and the message must not be resent
  (`gmi.Result.Accepted`).
- A send is idempotent on its draft id, the form's `message_id`, which
  compose.js keeps with the localStorage draft (a second copy in the
  sessionStorage marker; the submit is blocked if localStorage won't keep
  it). `SendLog` (`$XDG_STATE_HOME/pneu/sends/`, dir 0700, files 0600,
  temp + fsync + rename + fsync dir) holds a reservation for (account, id,
  payload hash) before gmi is handed anything, then the result: accepted,
  accepted-no-local-copy, unknown, or rejected. Only rejected frees the id,
  and only a failure that proves nothing went out is rejected: `ErrBusy`
  (gmi never started) or `gmi.Result.NotSent` (lieer exited on its own
  before its "sending message" line, from a scan of the whole output
  stream, never the tail). After that line, anything short of acceptance
  is unknown, whatever HTTP error follows. A reservation with no result is
  unknown too: "may have been sent", never resent. Same id and hash gets
  the recorded answer; a different hash is refused. The hash is the
  server's, over what it sends. Records are kept 30 days from the last
  submit (`SendKeep`; a replay refreshes it); a submitted draft keeps its
  id until discarded. Mailto unsubscribes use the same log, keyed by
  account, message and mailto. Known limit: httplib2 (lieer's transport)
  resends a POST whose connection dropped, so one `gmi send` can send
  twice; that is lieer's, not patched here.
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
