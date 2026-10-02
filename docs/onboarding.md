# Onboarding

Design for PLAN.md "Later → First-run onboarding". Nothing here is built yet.
Every lieer claim cites lieer 1.6 source (`/usr/lib/python3.14/site-packages/lieer/`,
`pacman -Q lieer` = 1.6-5) or google-auth-oauthlib 1.4.1 as `file:function`.

## Recommendation

- **No two-stage pull.** A single plain `gmi pull` already downloads newest
  first and commits every batch as it goes, so the inbox becomes readable a few
  minutes into the first pull, after the listing phase. A `--limit` first stage
  followed by a `pull -f` backfill gains nothing and adds a tag-revert hazard
  (below). lieer needs no changes.
- **The server owns the first pull.** The engine's "not pulled" guard becomes
  an account state machine. In the `pulling` state the engine runs `gmi pull`
  as a child with no timeout, parses lieer's non-TTY progress output, and
  broadcasts it over SSE.
- **The account is read-only while its first pull runs.** You can read it,
  but tag writes, mark-read and send are refused until the pull completes.
  This keeps the pull free of any interaction with pneu-touch or the revert
  hazard, and send can't get the lock anyway.
- **INSTALL.md stops at "authorized"; the app does everything after.** The
  agent does the one-time, auditable setup, including OAuth consent, through
  a new `pneu account` subcommand. The first pull and every later re-auth
  happen in the app.

## Findings from lieer's source

### Bounding a pull

- **No date or query filter.** The listing query is the class attribute
  `Remote.query = "-in:chats"`. `remote.py:Remote.all_messages` always sends
  it with `includeSpamTrash=True`, and no CLI flag sets it
  (`gmailieer.py:Gmailieer.main`).
- **`--limit` is a soft cap on the listing.** In
  `gmailieer.py:Gmailieer.full_pull` the listing loop breaks once the gathered
  ids reach the limit, checked after each page of 100 (the API default:
  `all_messages` passes `maxResults=None`).
- **`--limit` refuses to run with `remove_local_messages`, which is on by
  default** (`local.py:Local.Config`). `full_pull` raises `ValueError` *after*
  listing completes if `limit` is set and `remove_local_messages` is true. So a
  limited first stage needs `gmi set --no-remove-local-messages` first and
  `--remove-local-messages` again afterwards.
- **A limited full pull still completes the repository.** When it ends,
  `full_pull` stores `lastmod = db.revision()` and `last_historyId` (the value
  captured *before* listing). From then on every pull is partial
  (`Gmailieer.pull` branches on `last_historyId == 0`), and the missing older
  mail only arrives through a forced `pull -f`.

### What a later full pull does with mail already fetched

- **Content is never re-downloaded.** `Gmailieer.get_content` fetches only ids
  where `not self.local.has(m)`, and `has()` reads a cache that
  `Local.__load_cache__` builds from the filenames in `mail/cur` and `mail/new`.
- **Every message that was already local gets a metadata fetch, and its tags
  are overwritten with Gmail's.** `full_pull` computes
  `needs_update = message_gids - updated` (everything not freshly downloaded)
  and calls `get_meta`, which calls `local.py:Local.update_tags`. That function
  clears the message's tags and sets them to the remote labels plus ignored
  tags, with no check against lastmod. A local tag change that hasn't been
  pushed is silently reverted.
- **State is rewritten at the end.** `lastmod` becomes the database revision
  at the end of the pull and `last_historyId` becomes the id captured at its
  start. With `--resume` it is the id from the interrupted run, and a
  `partial_pull` follows to catch up.

So a two-stage pull (`--limit`, then `pull -f`) doesn't corrupt state or
re-download content. It does hold the lock for the entire backfill, exactly as
a single pull would. Any triage done on stage-one mail during the backfill is
at risk, because the backfill's `get_meta` visits those messages in `set()`
order and overwrites their tags. Stage one does buy a window of normal sync
between the stages, and it skips most of the listing phase. That isn't worth
the hazard and the flag juggling.

### Why a single pull is already "recent first"

- **Newest first.** `full_pull` keeps listing order in `message_gids`.
  `get_content` preserves that order when building `need_content`, and
  `Remote.get_messages` fetches it in order, in batches of up to 50. Gmail's
  list order is newest first. That's observed, not documented. Checked on
  disk: lieer stamps each file's mtime with the message's `internalDate`
  (`Local.store`). Sorting the smaller account's maildir by ctime (store time) gives
  decreasing mtime for 7,422 of 7,484 consecutive pairs.
- **Committed per batch.** `get_content._got_msgs` opens the database
  read-write for each batch and closes it after storing, so each batch is
  committed and visible to readers within seconds. The Xapian write lock is
  held only while that batch is stored. Between batches, readers and even
  writers get in.
- **Mail stored during the pull is final.** Freshly downloaded messages are in
  `updated`, so `get_meta` doesn't revisit them in the same run. Their tags
  come from the `raw` fetch's `labelIds` (`Local.store`, then `update_tags`).
- **Measured on the author's two accounts (journal, 2026-09-23).**
  - a 73,647-message account: Listing 19:51:13→19:54:24, content
    19:54:24→20:43:29, 6.1 GB.
  - a 7,498-message one: Listing 20 s, content 8 m 37 s, 1.6 GB.

  That's about 25 messages/s once content starts, so the most recent few
  thousand messages land within minutes of the listing ending.

### Progress signal

- **pneu always gets lieer's non-TTY output.** tqdm is used only when both
  stdout and stderr are TTYs (`Gmailieer.setup`). Otherwise it's
  `nobar.py:tqdm`, which is what pneu gets from a pipe.
- **nobar's format.** `__init__` prints `desc (total) ...` with no newline,
  `update` prints one `.` whenever the running count is divisible by 10, and
  `close` prints `done: N its in D`. Everything is flushed.
- **The phases, in order,** from `full_pull`, `get_content` and `get_meta`:

  | header | meaning | total | one dot = |
  |---|---|---|---|
  | `fetching messages (1) ...` | listing | unknown (`total=1` is a placeholder; tqdm gets `resultSizeEstimate`, nobar ignores it) | one page ≈ 100 ids |
  | `removing deleted (N) ...` | local files gone from Gmail (0 on a fresh repo) | exact | 10 |
  | `receiving content (N) ...` | downloading raw messages | **exact** | 10 |
  | `receiving metadata (N) ...` | label refresh for pre-existing mail (a restart only) | exact | 10 |
  | `pull: complete, removing resume file` | success | | |

  `receiving content: everything up-to-date.` and
  `receiving metadata: everything up-to-date.` stand in for a skipped phase.
- **Other output lands mid-line.** `remote: reducing batch request size to: 25`
  and its reverse arrive constantly (the 7,498-message pull printed about 180 of
  them), each ending the current dot line. `remote: waiting 1.0 seconds..`
  contains dots of its own. A parser counts only *leading* runs of `.` on each
  line, plus the run after a header's `...`.
- **No total up front.** The count is known only when listing ends: the
  `done: N` line of `fetching messages`, then the header of
  `receiving content (N)`. pneu can't ask Gmail itself (the app never talks to
  Google), and with no TTY there's no estimate either. The UI shows a count
  during listing and a fraction after it.
- **The resume file is thin.** `.resume-pull.gmailieer.json`
  (`resume.py:ResumePull.save`) holds only `{version, lastId, meta_fetched}`.
  `meta_fetched` fills only during the metadata phase, and a fresh first pull
  has no metadata phase, so the file carries no progress for a first pull.
  PLAN.md's "the resume file carries enough to show it" is wrong. The file's
  *presence* is still useful: `full_pull` creates it at the start of every full
  pull (`load_resume` → `ResumePull.new`) and deletes it only on success, so it
  means "a full pull started and didn't finish".

### OAuth

- **The flow.** `gmi auth` calls `remote.py:Remote.__get_credentials__`,
  which runs `InstalledAppFlow.run_local_server()` with **no arguments**.
- **Fixed port.** Defaults in `google_auth_oauthlib/flow.py:run_local_server`:
  `host="localhost"`, **`port=8080`** (fixed, not random), `open_browser=True`,
  `timeout_seconds=None`. lieer's `--auth-host-name`, `--auth-host-port` and
  `--noauth_local_webserver` flags are parsed and never passed
  (`Gmailieer.main` vs `Remote.__get_credentials__`), so they're dead.
  The old substrate doc's "random port" was wrong.
- **What `run_local_server` does, in order:** binds `localhost:8080`
  exclusively (fails fast if taken), calls `webbrowser.get(None).open(url)`,
  *then* prints `Please visit this URL to authorize this application: <url>`
  to stdout, and serves exactly **one** request (`handle_request`). PKCE is on
  (`autogenerate_code_verifier=True`), and `authorization_url` asks for offline
  access, so a refresh token comes back.
- **No browser means no URL.** `webbrowser.get()` raises
  `could not locate runnable browser` before the URL is printed if no browser
  can be found. Verified: with `env -i PATH=/usr/bin` it raises, and with
  `BROWSER=true` it "opens" successfully. Here the user systemd environment
  has `BROWSER=helium-browser` and the display variables, but a stranger's may
  not.
- **So the server can drive it.** Spawn `gmi auth` with `BROWSER=true`, read
  the URL line from stdout, hand it to the app window to open, and wait for
  the process to exit. The consent URL holds no secret; the PKCE verifier stays
  inside the gmi process.
- **`auth -f` deletes the credentials first** (`Remote.authorize(reauth=True)`
  unlinks `.credentials.gmailieer.json`), so an abandoned re-auth leaves the
  account with no credentials.
- **Without `-c`, lieer silently uses its built-in shared client** (the
  `else:` branch of `__get_credentials__`). Once credentials exist, `-c` no
  longer matters: refresh uses the client id and secret stored in the
  credentials file.
- **Dead tokens.** Refresh failure isn't caught: `credentials.refresh()` raises
  `google.auth.exceptions.RefreshError: ('invalid_grant: …')` and gmi exits
  non-zero with a traceback. That's the signature of the Testing-mode 7-day
  expiry, a revoked grant, and a password change on the Google account.

### lieer's shared client

`Remote.OAUTH2_CLIENT_SECRET` embeds a client (project `capable-pixel-160614`,
redirect URIs `oob` and `http://localhost`). Whether Google has verified it
isn't visible from source. The scopes it asks for (`gmail.readonly`,
`gmail.modify`) are Google "restricted" scopes: an app serving the public
needs verification plus a security assessment, and an unverified one is capped
at 100 users and shows the unverified-app interstitial. Its quota is also
shared by every lieer user. Don't build on it. INSTALL.md already has
strangers create their own Desktop client. It should also say:

- **gmail.com account:** use an *External* app **published to production**.
  Testing mode gives 7-day refresh tokens, which pneu would show as a
  "Reconnect" every week.
- **Workspace account:** use an *Internal* app in the org's project.
- **Keep the client JSON on disk** at `~/.config/pneu/<acct>/client_secret.json`.
  Re-auth needs it, and without it lieer falls back to the shared client.
- **Only the console steps are the human's.** The agent can't create an
  External consent screen or a Desktop client with `gcloud`; those are manual.
  It *can* validate the downloaded JSON (top-level `installed` key, a
  `client_id` ending `.apps.googleusercontent.com`) without printing
  `client_secret`.

### `pulled()`, staged pulls and pneu-touch

`pulled()` (`internal/gmi/gmi.go`) tests `last_historyId > 0`. lieer writes
that value only at the end of a full pull (`full_pull`), so with a single
first pull it flips exactly once, when the archive is complete. A `--limit`
stage would flip it early, and the engine would then run normal syncs on a
partial archive. That's another reason not to stage.

pneu-touch exists because a full pull ends with `set_lastmod(rev)`, which
swallows tags written during it. The first pull is a full pull, so the same
rule applies. With the account read-only during the first pull, nothing is
written and there's nothing to re-mark. `NoteWrite` keeps doing its job for
later full pulls: `--force` runs, and lieer's fallback when the stored history
id has expired (`partial_pull` catches a 404 and calls `full_pull`).

## Where the line falls

| Step | Where | Why |
|---|---|---|
| Packages, build, units, desktop glue | INSTALL.md | One-time and auditable: this is what the agent is for. |
| GCP project, consent screen, Desktop client | INSTALL.md (human in the console; the agent validates the JSON) | The app never talks to Google, and `gcloud` can't make an External client. |
| Dirs, notmuch config, `notmuch new`, `gmi init`, `gmi set`, config.json entry | `pneu account add` (run by INSTALL.md) | Deterministic file writes. One idempotent command replaces the error-prone half of INSTALL step 5. |
| First consent | `pneu account auth` (run by INSTALL.md) | `gmi auth` opens the browser itself from a terminal, so doing it in-app buys nothing on day one. The agent verifies the token right after. |
| First pull | **App** | Runs for 10 minutes to hours, longer than an agent session should babysit. It needs a real progress display, and the inbox is usable before it ends. |
| Resume after interruption or restart | **App** | Automatic; nobody should have to remember `--resume`. |
| Re-auth when a token dies | **App** | Happens months later with no agent around, and the app is where the failure shows. It uses the same code path as `pneu account auth`. |
| Adding a second account later | INSTALL.md steps again (open question 4) | In-app would need engine hot-add and a client-JSON upload. |

INSTALL.md changes this implies, for its author: steps 5 and 6 collapse to
`pneu account add` + `pneu account auth`, the service starts next, and the
detached `systemd-run … pull` goes away. Audit item 7 ("pneu never reads" the
credentials file) stays true under this design, since verification goes through
gmi.

## `pneu account`

Subcommands of the one binary (`pneu` with no subcommand serves). They share
paths and locking with the engine through `internal/config` and the
account's flock.

```
pneu account add <name> <address> [--client-secret FILE]
pneu account auth <name> [--force]
pneu account status [<name>]          # the state below, one line per account
pneu account pull <name>              # foreground first pull, same progress parser; for headless use
```

**`add` is idempotent.** Each step checks before acting and says what it did
or skipped.

1. **Name and address.** Validate the name (config's rules); refuse if the
   address already belongs to another account.
2. **Directories and notmuch config.** Create `~/mail/<name>/gmail` and
   `~/.config/pneu/<name>/`. Write `notmuch-config` with the non-default
   settings from INSTALL.md step 5. `user.other_email` gets the other
   configured accounts' addresses, and the existing accounts' configs are
   updated to list this one.
3. **Database.** `notmuch new` on the empty tree. lieer's `load_repository`
   opens the database and fails if it doesn't exist.
4. **Client JSON.** With `--client-secret`, copy it to
   `~/.config/pneu/<name>/client_secret.json` with mode 0600, after the shape
   check above.
5. **lieer init.** `gmi init --no-auth --replace-slash-with-dot <address>`,
   skipped if `.gmailieer.json` exists.
6. **Ignore pneu-touch.** `gmi set --ignore-tags-local`, merged with whatever
   is already listed (the flag replaces the list, `Local.Config.set_ignore_tags`).
7. **Register the account.** Add it to `config.json`, written atomically.

**`auth` runs gmi under the account lock.** It runs
`gmi auth [-f] -c <client_secret.json>` under the account's flock, always with
`-c`, and refuses if the JSON is missing rather than let lieer fall back to
the shared client. Before starting it checks that `localhost:8080` is free
(`net.Listen`, then close) so a busy port gets a clear error instead of a
Python traceback. After success it verifies with `gmi pull -t` (list labels):
that reads from Gmail as `<address>` and fails if consent was given as a
different Google user, the "account chooser" trap. It doesn't open or print
the credentials file.

## In-app architecture

### Account state

A pure function of lieer's files plus the engine's in-memory run state,
computed per request. Files are only `stat`ed, except the state JSON pneu
already reads.

```go
type State string

const (
	StateUnconfigured State = "unconfigured" // no .gmailieer.json: run `pneu account add`
	StateUnauthorized State = "unauthorized" // no .credentials.gmailieer.json (stat only)
	StateNeedsPull    State = "needs-pull"   // last_historyId == 0, no pull running
	StatePulling      State = "pulling"      // first pull in flight (Progress attached)
	StateReady        State = "ready"        // last_historyId > 0
	StateReauth       State = "reauth"       // ready, but the last run failed with invalid_grant
)
```

- **Interrupted is folded into needs-pull.** The resume file's presence only
  changes the next pull's arguments (`--resume`).
- **Failure is a field, not a state.** A pull that exits non-zero leaves
  `needs-pull`, with `LastErr` and backoff, exactly as today's `Status`.
- **`pulled()` and `ErrNotPulled` become `state()`.** `do()`'s early return
  turns into a switch:
  - ready: sync or push as now.
  - needs-pull: run the first pull (auto-start; open question 2).
  - anything else: idle, with the state reported.

### The first pull as an engine op

`OpPull` goes through the same slot and flock as every run, but differs from
`exec` in four ways:

- **No `SyncTimeout`; a stall watchdog instead.** If no output byte arrives
  for 10 minutes, kill the pull and retry with backoff. lieer's HTTP timeout
  setting is dead code: `Remote.authorize` computes `timeout` and never passes
  it to `discovery.build`, so a hung socket would otherwise hang forever.
- **Shutdown interrupts it.** Today's `exec` detaches from ctx so a sync
  finishes. A first pull would outlast `TimeoutStopSec`, so it gets SIGINT to
  the process group, then SIGKILL after `WaitDelay`. SIGINT raises
  `KeyboardInterrupt` in Python, so the `with notmuch2.Database(...)` block
  closes cleanly.
- **Before starting:**
  - Empty `mail/tmp/`. `Local.store` writes to `tmp/` and renames into
    `cur/`, and raises `RepositoryException("local temporary file already
    exists")` on any leftover, so a kill between those two steps would wedge
    every later pull on that message. With the flock held and gmi not running,
    everything in `tmp/` is an orphan.
  - Pass `--resume` if `.resume-pull.gmailieer.json` exists. It's harmless
    when the stored history id has expired: `full_pull` detects that and
    starts over.
- **Output.** stdout and stderr are teed into the tail buffer and the progress
  parser. On success the engine queues a normal sync right away: new mail
  from during the pull arrives through `partial_pull` from the history id
  captured at the pull's start.

A restart mid-pull (every deploy) costs a fresh listing plus a metadata pass
over what's already stored. No content is re-downloaded. Nothing local is
reverted, because the account was read-only.

### Progress

```go
// Progress is derived from lieer 1.6's non-TTY output (nobar.py): a header
// "desc (total) ...", one '.' per 10 items (per page while listing), and
// "done: N its in D". Display only: on anything unrecognised it goes
// indeterminate, never wrong.
type Progress struct {
	Phase    string    // listing | removing | content | metadata
	Done     int       // dots×10 (content, metadata), dots×100 (listing); exact after "done:"
	Total    int       // 0 while listing
	Frontier time.Time // date of the oldest message stored so far
	Rate     float64   // items/s over the last minute
	Updated  time.Time
}
```

- **Parser.** An `io.Writer`:
  - Headers match `^(fetching messages|removing deleted|receiving content|receiving metadata) \((\d+)\) \.\.\.`.
  - Only a leading run of `.` counts, after a header or at the start of any
    line, so the dots inside `remote: waiting 1.0 seconds..` don't.
  - `done: (\d+) its` snaps `Done` to the exact figure.
- **Frontier.** Newest-first storage makes "complete back to <date>" true,
  and more useful than a percentage: it tells you whether an old inbox message
  should be there yet. About every 30 s during content, pneu reads the oldest
  stored message:
  `notmuch search --format=json --output=summary --sort=oldest-first --limit=1 '*'`.
  That's a read, so it doesn't contend for the write lock.
- **Broadcast.** SSE `account`, at most once a second:
  `{account, state, phase, done, total, frontier, rate, error}`.

### Read-only while pulling

- **Tag endpoints.** The handlers check the account's state and answer 409
  with "<account> is still downloading; changes unlock when it finishes" for
  any id list touching a non-ready account. Ids from a ready account in the
  same request go through. Opening a thread skips mark-read for that account.
- **Compose.** Compose hides a non-ready account's From. Send couldn't run
  anyway: `gmi send` takes lieer's repository lock with `block=True`
  (`Gmailieer.send` → `setup(..., block=True)`), and the engine slot is held
  for the whole pull.
- **Manual runs.** `pneu gmi <acct> …` waits 10 minutes on the flock and then
  fails. `pneu account status` says why.

### Re-auth

The trigger is a ready account whose last run's output contains
`invalid_grant`, which moves it to `StateReauth`. The banner reads "<account>
lost access to Gmail. Reconnect".

1. `POST /accounts/<name>/reauth` takes the account slot and flock and checks
   port 8080. It spawns `gmi auth -f -c <client_secret.json>` with
   `BROWSER=true`, reads stdout until the `Please visit this URL…` line, and
   answers `{url}`. The client opens it with `window.open`, which lands in a
   helium tab.
2. The process finishes when Google redirects to `localhost:8080`. The engine
   then runs the `gmi pull -t` check and a normal sync, and broadcasts SSE
   `auth {account, ok, error}`.
3. `POST /accounts/<name>/reauth/cancel` sends SIGINT to the process. A
   10-minute cap does the same (`run_local_server` has no timeout).

Offer re-auth only from `StateReauth` (or `unauthorized`): `-f` deletes
working credentials before the flow starts. The same function backs
`pneu account auth`.

### Endpoints

All mutations are POST behind the existing Host, Origin and session
middleware. Account names are validated against config, never used as paths
directly.

| Method | Path | Does |
|---|---|---|
| POST | `/accounts/<name>/pull` | Start or retry a first pull (409 unless `needs-pull`). |
| POST | `/accounts/<name>/reauth` | Start consent, return `{url}`. |
| POST | `/accounts/<name>/reauth/cancel` | Abort it. |

Account state and progress are rendered into the page and then pushed by SSE
`account` events. No new GET endpoint is needed.

### UI

- **No ready account.** The list area is an onboarding panel instead of the
  empty list's watermark, with one block per account:
  - `unconfigured` / `unauthorized`: the exact `pneu account …` command to
    run. The app can't do these steps.
  - `pulling`: a phase line and a meter, e.g. "Listing — 41,200 found", then
    "Downloading 12,340 of 73,647 · complete back to March 2024 · 28/s". The
    list fills in underneath as batches land, via the existing `sync`-style
    refresh fired on a throttle.
  - Failure: the last error line and a Retry button.
- **Some accounts ready.** Normal UI. A pulling account gets a one-line strip
  in the header (phase + meter), its rows are read-only (keys show the 409
  message), and it's missing from the compose From list.
- **Reauth.** A header banner with a Reconnect button.

## Gotchas

- **Threads are partial during the pull.** Recent messages arrive first, so a
  long thread shows its newest messages before its older ones. The frontier
  date is the honest signal. Don't claim a thread is complete.
- **Reads race lieer's commits every couple of seconds for the whole pull.**
  Normal syncs commit rarely, so a read path that has never hit Xapian's
  "database modified" error may start hitting it. The notmuch read helpers
  should retry once on that stderr.
- **Port 8080 is common.** A dev server on it makes `gmi auth` fail. Pre-check
  and name the port in the error.
- **Tabs can reach the callback.** `run_local_server` answers exactly one
  request on 8080, and any tab can hit that port while consent is open. A stray
  request ends the flow with a state/code error (PKCE and `state` stop a
  forged code from succeeding), so this is denial of service, not a takeover.
  Surface the error and let Retry start over.
- **Refresh token on re-auth is unverified.** Whether Google returns a
  refresh token when the same client re-consents after `invalid_grant` hasn't
  been checked. `authorization_url` asks for offline access but doesn't send
  `prompt=consent`. The `gmi pull -t` check proves only a working access
  token. Confirm on the first real re-auth that sync still works after an
  hour.
- **`config.Default()` hardcodes the author's two accounts.** A stranger without
  `config.json` would get them. `pneu account add` should own config.json,
  `Default()` should become zero accounts, and the server should start with
  zero accounts (today `config.normalize` and `gmi.New` both reject that) and
  render the onboarding panel.
- **Disk use isn't known up front.** Here it was 6.1 GB for 73k messages.
  After listing, pneu could project it from the average size of what's stored
  so far times the total, and pause with a message if free space
  (`syscall.Statfs`) falls below a floor. See open question 5.
- **Two earlier claims were wrong,** both since fixed: the old substrate
  doc's "random port" (it's 8080; INSTALL.md says so) and PLAN.md's "the
  resume file carries enough to show it".

## Decisions (2026-09-25)

1. **Read-only during the first pull.** No op journal.
2. **The first pull starts automatically** when an account reaches needs-pull.
3. **The pull is a child of the server**; progress is parsed from its pipe. A
   deploy restarts it (re-listing plus a metadata pass, no content).
4. **Adding accounts in the app is deferred.** `pneu account add` via
   INSTALL.md is the only way in.
5. **Stall watchdog: kill after 10 minutes of silence.** No disk-space check.
6. **No upstream lieer patch** unless the read-only hour turns out to hurt.

## As built (2026-09-25)

What the build settled that the design above left open:

- **Read-only means "no completed first pull, or one running".**
  `Server.readOnly` refuses tags, undo, mark-read and send for any account
  whose lieer state has no history id, or whose first pull is running. The
  second case matters because a resumed pull records its history id before
  its closing partial pull. It also covers an account whose token died
  before its first pull finished. A pulled account whose credentials are
  missing stays writable. The thread page omits its
  unread ids, and compose lists it as disabled.
- **Only in-app failures get in-app re-auth.** Reconnect is offered only in
  `StateReauth`, which is any state but unconfigured once a run's output
  says `invalid_grant`. That includes the case where an abandoned in-app
  re-auth has already deleted the credentials (`auth -f`), so the way back
  stays in the app. An account that was never authorized stays a terminal
  step (`pneu account auth`).
- **Re-auth checks before and after.**
  - Before: holding the account's slot (taken without waiting) and flock,
    Reconnect first runs `gmi pull -t` with the existing credentials. If they
    work, for example because they were fixed in a terminal meanwhile, it
    clears the failure and answers "already connected". Only `invalid_grant`
    goes on to `gmi auth -f`, which deletes them.
  - After: once consent is given, the same check runs before the account is
    called healthy.
  - One re-auth runs at a time.
  - The consent URL must start `https://accounts.google.com/`, and
    `pneu account add` accepts only a client JSON whose `auth_uri` and
    `token_uri` are Google's.
  - The re-auth's gmi takes Run's context, and Run waits for it, so a
    stopped server leaves nothing holding port 8080.
  - `pneu account auth` holds one lock across its consent and its check.
- **Stall means silent and not reading.** The watchdog stops a first pull
  only after 10 minutes with no output and no bytes read by its process
  group (`rchar` in `/proc/<pid>/io`). lieer prints a batch of 50 messages
  only once the whole batch response is in, which on a slow link with big
  attachments can take longer. Its clock starts when the pull holds the
  lock, not while it waits for it.
- **mail/tmp is left alone under a bare `gmi`.** When lieer's own `.lock`
  (an fcntl lock, invisible to flock) is held by another process, the
  cleanup is skipped until a later pull.
- **Retries use the sync backoff.** A failed or stalled first pull is
  retried on the ordinary schedule: 30 seconds, doubling to 15 minutes. The strip's
  Retry now (`POST /accounts/{name}/pull`) queues one at once. Pushes are
  skipped while an account is unpulled.
- **Unset accounts are polled every 5 seconds.** An unconfigured or
  unauthorized account looks again every 5 s instead of every 30 seconds.
  So for an account the running server already has, the first pull starts
  within seconds of `pneu account auth`.
  - Accounts are fixed when the server starts; hot-add stays deferred
    (decision 4). An account added while the server runs needs a restart.
  - `pneu account add` and `pneu account auth` notice a running server,
    from a fresh status file or a pneu answering on the port, and say so;
    they never restart it themselves.
- **Progress surfaces:**
  - SSE `account`, once a second, whenever something changed.
  - The status file, only when the state, the phase or a whole percent
    changes.
  - The frontier (`notmuch search --sort=oldest-first`), every 30 s during
    the content phase.
- **Bar widget.** A first pull isn't a warning: the widget shows its percent
  (`…` while listing) in place of the unread count. Re-auth, unconfigured
  and unauthorized are warnings.
- **The consent window.** It opens blank on the click, is cut from its
  opener, and is navigated once the server answers, so popup blockers don't
  interfere.
- **`pneu account add` takes `--name` and `--client-secret`.** INSTALL.md's
  step 4 no longer places the JSON by hand. `pneu account status` reads the
  running server's status file when it's fresh.
- **Not built:** `pneu account pull`, the design's terminal-only first pull.
  The server does it, and `pneu account status` watches it.
