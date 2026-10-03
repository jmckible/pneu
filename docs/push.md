# Push sync: Gmail watch over Pub/Sub

Built (2026-10-03), optional per account: INSTALL.md "Instant mail" sets
it up. Planned after two Codex design rounds, first round C1–C19, second
round K1–K12; the decisions below are the plan, and the "As built"
sections say where the code went further. The live measurement (Build,
step 6) is still to come. The 30-second poll (4942804) wasn't enough: the
watch buzzes within seconds of Gmail's receipt, pneu hears up to 30s
later, and opening the window still waits on a sync. Goal: mail lands in
notmuch, and the bar's count moves, within a few seconds of arrival, with
the window closed. Optional per account; everything works as today
without it.

## Shape

```
mailbox ──users.watch (mailbox's gmail.metadata token)──▶ topic pneu-<account>
                                                              │
                                                  subscription pneu-<account>
                                                              ▲
pneu server ── POST :pull ×2 outstanding (owner's pubsub token) ── message ──▶ Engine.Nudge(account)
            └─ POST :acknowledge
                         all in ONE dedicated GCP project: the push project
```

- **Pull, not push.** A push subscription needs an endpoint Google can
  reach (Funnel, cloudflared); pull is outbound only. Unary REST pull
  doesn't promise low latency (C3), so two pulls stay outstanding per
  account; the live step measures it. StreamingPull is gRPC, out of reach
  without a dependency.
- **A message is a nudge.** A message on an account's subscription means
  "sync that account soon": `Engine.Nudge(account)` (D5). The body
  (`{emailAddress, historyId}`) is never decoded. Nothing from Google
  reaches a command, a page, a log or status.json except through the
  closed vocabulary (D6). A forged or replayed message can only cause a
  sync.
- **The poll stays exactly as it is** (C18): 30s, the engine's failure
  backoff unchanged. Push adds syncs; it never slows the poll.
- **Server only.** A client sees the result through the link's existing
  events; setting push up from a client runs on the server over SSH.
- **One server per push project** (K9). Gmail's watch has no installation
  identity and `users.stop` takes none, so two servers would fight over
  each mailbox's watch. Each subscription is labelled with this server's
  install id; setup refuses one labelled by another.

## Decisions

### D1. A dedicated push project

One new GCP project, **the push project**, owned by one identity of the
user's (the **owner**; INSTALL.md suggests a plain gmail.com account kept
for this, which can create a project with no organization, holds no other
Pub/Sub, and isn't itself a pushed mailbox), with
its own Desktop OAuth client (External, In production, same branding
route as INSTALL.md step 4) used for nothing but push, the Gmail and
Pub/Sub APIs enabled (console, once), and every pushed account's topic
and subscription.

- **Revocation** (C6): revoking a grant revokes that user's scopes for
  the whole project. lieer's projects are separate, so nothing push does
  touches lieer. Within the push project the owner's two roles aren't
  separable (K5): if the owner is also a pushed mailbox, revoking either
  grant ends both (a dedicated gmail.com owner avoids it). pneu never revokes (D4 `--off`), and says this
  wherever it points to the permissions page.
- **Project identity** (C2, K6): the project ID is given once to `pneu
  push init`, validated (`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`), and must equal
  the client JSON's `project_id`. The client ID is recorded; a different
  client later is refused (re-init is an explicit `--replace`).
- **Reach** (C7): Pub/Sub access is the owner's alone. A Workspace
  mailbox grants pneu only `gmail.metadata`, so no token pneu holds
  reaches that org's Pub/Sub. The owner should be an identity with no
  unrelated Pub/Sub; INSTALL.md says so.
- `users.watch` requires the topic in the calling client's project,
  which is the push project for every mailbox. Cross-org watch through an
  External client is consistent with Google's documented rules; the live
  step confirms it for the Workspace mailbox.
- A project under a Workspace org may inherit domain-restricted sharing,
  which **may** refuse the `gmail-api-push@system.gserviceaccount.com`
  publisher binding (C19). A Workspace identity can't create a project
  without an organization (the console lists "No organization" but won't
  take it), so the push project usually sits in the owner's own org; the
  setup error names the policy, and INSTALL.md gives the project-level
  override (`iam.allowedPolicyMemberDomains` → Allow All on this project
  only). A Workspace mailbox's admin may need to trust the push client
  for `gmail.metadata` (Admin → Security → API controls).

### D2. Credentials and the state file

Two kinds of token, both pneu's, never lieer's:

- **Owner**: scopes `openid email https://www.googleapis.com/auth/pubsub`.
  Its identity is the ID token's `sub` from the token endpoint (received
  directly over TLS, so OIDC permits skipping the signature; `iss`
  `https://accounts.google.com`, `aud` our client ID, `exp`, `nonce`
  checked), pinned at init (K6). Setup and run-time pull/ack.
- **Mailbox**, one per pushed account: scope
  `https://www.googleapis.com/auth/gmail.metadata`. `getProfile` must
  return the configured address (C8), and no two accounts may hold the
  same address (one topic per mailbox). `watch`, `stop`.

Every token response (exchange **and** refresh, K7) must carry exactly the
expected scope set, `token_type` Bearer, `expires_in` in (0, 24h]; an
exchange must carry a refresh token. Anything else is an error, never a
degraded mode. The callback's `scope` is never read.

**One file holds all push state** (K1, K3, K4): `$XDG_STATE_HOME/pneu/push/state.json`.
config.json is not touched by push.

```json
{"version":1,"generation":7,"project":"pneu-push-123","install":"9f3c1a2b",
 "client":"<client id>",
 "owner":{"sub":"…","email":"…","refresh":"…","granted":"…"},
 "accounts":{"personal":{"state":"on","address":"…","refresh":"…","granted":"…"},
             "vocal":{"state":"off-pending","address":"…","refresh":"…","granted":"…"}}}
```

- Account `state` is `on` or `off-pending` (disabled locally, `users.stop`
  not yet confirmed; the token is kept for that one call).
- Dir `push/` exactly 0700, ours, `Lstat`, no symlink. `state.json`,
  `client.json` (the push client JSON, copied in by init) and the lock
  files: regular, ours, 0600, O_NOFOLLOW, size-capped (64 KiB / 16 KiB),
  checked on every open. Parsed by token (each key once, nothing after,
  every field bounded), like `update.json`.
- Written only by the CLI, under `push/push.lock` (never replaced): read,
  modify, `generation+1`, temp + fsync + rename + fsync dir. The hash is
  SHA-256 over the file's bytes. The daemon only reads it; a rename
  gives it a whole snapshot without a lock.
- A refresh never writes the file (Google doesn't rotate refresh tokens);
  access tokens live in the daemon's memory, per generation.

### D3. Consent

pneu builds the URL: `https://accounts.google.com/o/oauth2/v2/auth`,
`access_type=offline`, `prompt=consent`, `include_granted_scopes=false`,
PKCE S256, 128-bit `state`, (owner) a `nonce`, `login_hint`, redirect
`http://localhost:8080/`. It passes `gmi.ValidConsentURL`. One consent
session object holds state, verifier, nonce, redirect and client.

- **On the server:** pneu binds 127.0.0.1:8080 and [::1]:8080 and
  **holds** them until the callback (C9), with the relay's Host, method,
  path, size, time and one-shot checks, factored out of `relay.go`. The
  bind is Go's default (SO_REUSEADDR), like the client relay's: built
  first with lieer's rule (none), it refused for a minute after every
  consent's redirect left a TIME_WAIT, so back-to-back setups stalled
  (found live, 2026-10-03). On Linux SO_REUSEADDR still never binds over
  a listening socket, so a waiting consent, lieer's or pneu's, refuses
  it, and lieer's bind refuses ours while we hold the port. A bind
  failure is "a consent is already waiting (lieer's or pneu's)".
- **From a client** (K8): the relay forwards the callback line as today.
  The server, not the relay, is authoritative: it checks `state` against
  its own session in constant time and exchanges only `code` with its own
  verifier and redirect. EOF on stdin before the callback cancels the
  consent; EOF after it is normal. The relay's own state check moves to
  constant time too.
- `error=` is a denial result, worded locally, never Google's text.
- Consent runs **outside** `push.lock` and every engine lock; the
  transaction (D4) takes the lock only to commit, and re-reads the state
  under it.
- A stored, working credential for the same account and address is
  reused (K7): no consent unless `--reconsent`.

### D4. Ownership: the CLI writes state, the daemon runs it

**Only the daemon pulls and watches** (C12). The CLI does consent,
provisioning and `state.json`, then hands over over the control socket.

**Daemon lifetime lock** (K3): the daemon takes an flock on
`push/daemon.lock` (never replaced) before it starts any worker and holds
it for life. The CLI decides "no daemon" only by taking that lock itself
(non-blocking), never from a missing socket.

**Control commands** (declared in `internal/control`, fixed shapes like
`peers-reload`):

- `push-reload <gen> <hash>`: the daemon reads `state.json`; if its
  generation and hash match, it applies it: workers for accounts no longer
  `on`, or whose credential or the owner's changed, are cancelled **and
  joined**; new ones started; then it acks `ok <gen>`. A generation older
  than the applied one acks `stale`; a mismatched hash is `mismatch` (the
  CLI re-reads and retries). The handler makes no network call; joining a
  cancelled worker is bounded by request cancellation (≤ 2s), so the ack
  fits the 5s deadline.
- `push-state <account>`: one JSON line, by token: daemon instance,
  applied generation, and the account's health (D5).

**`pneu push init --project <id> --client-secret <json>`** (once): copy
the client JSON in, check `project_id`, owner consent, pin `sub`, check
`pubsub.topics.list` on the project works (a permission probe, not an
identity check), write the first `state.json`.

**`pneu account push <name>`**:

1. Mailbox consent (unless reusable); `getProfile` must match; no other
   account holds the address.
2. Provision with the owner token, idempotent (C5):
   - topic `pneu-<account>`;
   - publisher binding: `getIamPolicy` with `requestedPolicyVersion: 3`,
     add `roles/pubsub.publisher` for
     `serviceAccount:gmail-api-push@system.gserviceaccount.com`
     unconditionally, keep every other binding and condition as read (raw
     JSON round-trip of what we don't model; anything we can't preserve
     fails closed), `setIamPolicy` with etag and version 3; on an etag
     conflict re-read and recompute, at most 3 times;
   - subscription `pneu-<account>`, label `pneu-install: <install>`,
     pull, `ackDeadlineSeconds: 30`, `messageRetentionDuration: "3600s"`,
     `expirationPolicy: {}` (never; omitted means 31 days), no filter,
     dead-letter, transform or detachment. An existing one must match all
     of that, label included, or it's an error naming the field.
3. Commit under `push.lock`: account `on` with its token.
4. `push-reload` and wait for `ok <gen>`.
5. Wait up to 60s on `push-state` for **this** instance and generation to
   report `delivering`: it renewed the watch and then received a message.
   A watch publishes one at once (C4). On a reused subscription that's
   reported as "delivering", not as proof of this watch. A timeout leaves
   everything in place, says what's unproven, and re-running is safe.

**`pneu account push <name> --off`** (K2):

1. Commit `off-pending` (token kept).
2. `push-reload`, wait for `ok <gen>`: the worker is gone. With no ack:
   stop here, "pending; the daemon applies it when it answers", nothing
   deleted. No daemon (we hold `daemon.lock`): go on.
3. `users.stop` with the mailbox token.
4. Commit the account's removal. If `stop` failed, it stays
   `off-pending` and the output says remote cleanup is pending
   (re-running `--off` retries it; the watch also lapses by itself
   within 7 days).
5. Topic and subscription stay; the output says how to delete them, and
   where to revoke the grant, with D1's owner caveat.

The daemon at start reads `state.json` and runs only `on` accounts; an
`off-pending` account has no worker whatever happens in between.

### D5. Run time: `internal/push` in the daemon

A manager with its own context (K11), separate from the engine's. One
worker per `on` account.

- **Pull:** two outstanding `:pull` (`maxMessages: 10`), each with a 90s
  client timeout. A 2xx with messages: `Engine.Nudge(account)`, then ack
  those ackIds on the same subscription (≤ 10, each ≤ 512 bytes). An
  empty 2xx re-pulls, at most once a second per pull.
- **`Engine.Nudge(account)`** (K10), new in `internal/gmi`: sets the
  account's bounded nudge flag and returns at once; acceptance into it is
  when push acks (a crash loses at most a sync the poll covers). The
  account loop turns a pending nudge into a sync when it's eligible: at
  least 5s since the last sync started, and not inside the failure
  backoff the engine already computes (`delay`). Shared by both pulls by
  construction. A sync for any reason clears the flag.
- **Watch** (C14): renewed at start, then from the response's
  `expiration` (parsed as bounded decimal ms): next at
  min(now + 24h, expiration − (expiration − now)/2), with its own backoff
  (30s doubling to 1h) on failure. A pull error says nothing about the
  watch.
- **Tokens:** the manager owns refresh for the owner token (shared by all
  workers) and each mailbox token; refreshed when < 5 min remain by wall
  clock, scopes checked (D2). `invalid_grant` on a mailbox token stops
  that worker (`reauth`/`mailbox`); on the owner token, every worker
  (`reauth`/`owner`).
- **Backoff** for pull errors: 2s doubling to 5 min, reset after 5 min
  without one.
- **Wake** (K11): main.go's wake handler calls `manager.Woke()`. The
  manager bumps an epoch (any response from an older epoch is discarded),
  cancels in-flight requests, drops access tokens, and waits the same
  settle (`wakeSync`) before pulling and renewing watches. It adds no sync
  of its own (main.go's `Launch` covers that).
- **Shutdown** (K11): stop taking reloads (the control socket closes
  first, as now), cancel and join the manager, then let the engine stop.
  The manager is started after the control socket and stopped on every
  exit path from `serve`, deferred right where it starts.
- **Health** (K12), recomputed on every event and on a timer for the
  time-based transitions:
  - facts: `lastPullOK` (last 2xx pull, empty or not), `watchExp`,
    `lastDelivery` (last message), `reason`.
  - state, first match wins: `reauth` (owner or mailbox `invalid_grant`)
    → `failing` (no 2xx pull for 3 min, or now ≥ `watchExp`) → `starting`
    (no 2xx pull and no watch since this worker started) → `delivering`
    (a message within 24h) → `listening` (no message yet, the worker
    younger than 24h; added live: `lastDelivery` lives in memory and
    renewing a live watch publishes nothing, so every restart read
    `quiet` until the next mail) → `quiet` (nothing for 24h).
  - `reason`, closed: `owner-reauth`, `mailbox-reauth`, `api-disabled`,
    `permission`, `org-policy`, `network`, `watch-expired`, `unknown`.

### D6. The Google boundary

- One `http.Client`, three fixed hosts (`oauth2.googleapis.com`,
  `gmail.googleapis.com`, `pubsub.googleapis.com`), plus
  `accounts.google.com` only as a URL handed to the browser. Normal
  certificate verification, no redirects, no proxy from the environment
  (C10). Bearer tokens in headers; code, verifier, refresh token and
  client secret in POST bodies only.
- Response bodies capped (64 KiB), decoded into fixed structs; IAM
  policies round-trip their unknown fields untouched but are never shown.
- Scopes don't grant IAM (C1): each failure is classified separately into
  a closed local vocabulary: `scope`, `permission`, `api-disabled`,
  `org-policy`, `not-found`, `conflict`, `invalid-grant`, `unauthenticated`
  (an API call's access token refused, HTTP 401: drop it, refresh,
  retry; only the refresh's `invalid-grant` means reauth), `quota`,
  `unavailable`, `network`, `unknown`, from the HTTP status, OAuth `error`
  codes and `error.details[].reason`, matched against fixed strings.
  Nothing Google wrote is logged, printed or sent in a remote event; this
  layer's error type carries only the code and the operation.
- Hosts are constants; tests swap a base URL through an unexported hook,
  never config or env.

### D7. Surfaces (C17, K12)

- The account view (`data-accounts`, SSE `account`, hello) and
  status.json gain `push: {state, reason, lastDelivery}` (absent = off;
  `reason` only with `reauth`/`failing`). It never feeds `sick`: the bar
  doesn't turn red over push. A change of push state alone triggers the
  status write and the SSE event (accounts.go's change marker), under the
  same locks as hello.
- The client's two reconstruction paths (`internal/client/events.go`) and
  `StatusV2` carry it field by field: enums checked, time bounded, absent
  allowed. `shell/status.js` tolerates it already; its tests add it.
- Sync details: "Instant: delivering · last message 2m ago" / "quiet" /
  "off" / "failing: <reason in words>" with the fix: `pneu account push
  <name>` (mailbox), `pneu push init --reconsent` (owner). The polling
  note reads the engine's real next delay, not a hardcoded 30s.

## Build

Each task: an Opus subagent in the `../pneu-push` worktree (branch
`push-sync`), its own Codex review (fresh thread) before it's accepted,
fixes back to the same agent. Full suite (`go test -race ./...`, node,
qmltestrunner where touched) at every merge into the branch.

0. **Push project (human, any time):** INSTALL.md step 4's screens for a
   new project, plus the Pub/Sub API. Needed only by step 6.
1. **Foundations** (freezes everything 2 and 3 share):
   - `internal/google`: OAuth (consent session, URL, PKCE, exchange,
     refresh, ID-token claims, scope checks), Gmail (`getProfile`,
     `watch`, `stop`), Pub/Sub (topic, IAM v3 read-modify-write,
     subscription create/compare, list probe, pull, ack), the error
     vocabulary, the fixed-host client, all context-bound;
   - `internal/push/state`: D2's file, lock, generation, hash, the
     daemon lock;
   - `internal/control`: `push-reload` / `push-state` wire format,
     client functions and handler hooks (handlers unimplemented: task 2);
   - `Engine.Nudge` in `internal/gmi`, implemented and tested;
   - `googletest`: a fake of every Google call with injectable failures,
     a scripted message feed and a fake clock.
2. **Daemon** (needs 1): the `internal/push` manager (D5), the control
   handlers, main.go's `serve` wiring (start, wake, shutdown), D7's
   server-side surfaces (account view, status.json, change marker).
3. **CLI** (needs 1; parallel with 2): `pneu push init`, `pneu account
   push [--off]` local and remote (`remote.Verb`s, stdin schemas, EOF
   rule), the callback listener factored out of `relay.go`, D4's
   transactions. Its commands live in `cmd/pneu/push.go`; its only
   main.go edit is the dispatch line.
4. **Client and page** (needs 2): the client sanitizers and `StatusV2`,
   the sync details text, status.js tests.
5. **Install** (needs 3): INSTALL.md optional section "Instant mail"
   (push project screens, owner choice, org-policy and API-controls notes),
   audit items for the new outbound hosts and the push state dir, AGENTS.md
   contract lines, PLAN.md's "doesn't talk to Google" rewritten.
6. **Live** (human): `push init`, both accounts, measure buzz-to-bar over
   a day, decide whether two outstanding pulls are enough.

Adversarial cases the tests must hold (C19, K2, K3, K10): continuous
notifications and ack failures, repeated redelivery, nonempty floods,
early empty pulls, stalled HTTP, watch expiry across suspend, an old
epoch's response after a wake, renew OK + pull failing, `--off` racing a
renewal or refresh, setup and `--off` interrupted at each step, lost
reload ack, stale and mismatched reloads, daemon restart with
`off-pending`, two accounts with one address, first pull with nudges
arriving, lieer's consent waiting on 8080 during push consent.

## Interfaces (frozen by task 1)

What tasks 2 and 3 build on. Change one only with the other tasks told.

**`internal/google`** (one `*API` per process; safe for concurrent use;
every call takes a context and returns `*google.Error{Op, Code}` or the
context's own error; `google.CodeOf(err)`; `CodeUnauthenticated` on an API
call means drop the access token and refresh):

```go
api := google.New(google.Options{Now, Timeout, PullTimeout}) // 30s / 90s defaults
api.CloseIdle()                                                  // after a wake

c, _ := google.NewConsent(creds google.Credentials, google.Owner|google.Mailbox, loginHint)
c.URL()                       // passes gmi.ValidConsentURL; c.State() for the client relay
code, err := c.Callback(q)    // q from remote.ParseCallback; ErrState | ErrDenied | ErrCallback
tok, err := api.Exchange(ctx, c, code)          // Token{Access, Expiry, Refresh, Identity *{Sub, Email}}
tok, err := api.Refresh(ctx, creds, kind, refresh, pinnedSub) // sub "" for a mailbox; Refresh always ""

addr, err := api.Profile(ctx, mailboxAccess)
exp, err := api.Watch(ctx, mailboxAccess, project, topic)     // time.Time from bounded decimal ms
err = api.Stop(ctx, mailboxAccess)

res, ok := google.Resource(account)                           // "pneu-<account>": topic and subscription ID
err = api.ProbeTopics(ctx, ownerAccess, project)
created, err := api.EnsureTopic(ctx, ownerAccess, project, res)
changed, err := api.GrantPublisher(ctx, ownerAccess, project, res)  // IAM v3 RMW, ≤ PolicyAttempts
created, err := api.EnsureSubscription(ctx, ownerAccess, project, res, res, install) // *MismatchError{Field, OtherInstall}
err = api.CheckSubscription(ctx, ownerAccess, project, res, res, install)
ackIDs, err := api.Pull(ctx, ownerAccess, project, res)       // ≤ 10 IDs, bodies never decoded
err = api.Ack(ctx, ownerAccess, project, res, ackIDs)
```

Validators: `ValidProject`, `ValidClientID`, `ValidSecret`, `ValidAddress`,
`SameAddress`, `ValidSub`, `ValidInstall`, `ValidRefresh`. Codes:
`google.Codes`; ops: `google.Ops`.

**`internal/google/googletest`**: `f := googletest.New(t)`; `f.API()` /
`f.NewAPI(opts)` (on `f.Clock`); `f.Credentials()`, `f.Project`;
`f.AddUser(email)`, `f.Approve(consentURL, email)` / `f.Deny(url)` (the
callback query), `f.Revoke(email)`, `f.MutateNextToken`,
`f.MutateNextClaims`; failures: `f.Fail(op, Failure{Status, Body, Drop,
Stall, Times})`, `f.FailCode(op, code, times)`, `googletest.Answer(op,
code)`, `f.ClearFailures()`; the log: `f.Calls(op)`, `f.Requests()`;
Gmail: `f.Watch(email)`, `f.Stops(email)`, `f.Notify(email)` (new mail);
Pub/Sub: `f.AddTopic`, `f.HasTopic`, `f.SetPolicy`, `f.Policy`,
`f.PolicyWrites`, `f.RacePolicy(topic, n)`, `f.SetSubscription`,
`f.Subscription`, `f.Publish(sub, n)`, `f.Redeliver(sub)`, `f.Queued`,
`f.Outstanding`, `f.Acked`, `f.PullHold`; clock: `f.Clock.Now/Advance/Set/After/Waiters`.

**`internal/push/state`**:

```go
s := state.Store{Dir: state.Dir(web.StateDir())}
snap, err := s.Load()                 // Snapshot{File, Hash}; ErrNoState; no lock (the daemon's read)
l, err := s.Lock(ctx)                 // push.lock; l.Unlock()
snap, err := l.Update(func(f *state.File) error { … }) // generation+1, validated, atomic; Generation 0 = new
snap, err := l.Current()
c, err := l.WriteClient(clientJSONBytes) // Client{ID, Secret, Project}; c.Credentials()
c, err := s.LoadClient()
err = state.CheckClient(snap.File, c)  // same project and recorded client
d, err := s.TryDaemonLock()            // ErrDaemonRunning; d.Unlock()
running, err := s.DaemonRunning()      // probe only
id := state.NewInstall()
```

`File{Generation, Project, Install, Client, Owner{Sub, Email, Refresh,
Granted}, Accounts map[string]Account{State On|OffPending, Address,
Refresh, Granted}}`.

**`internal/control`**: `Handler.PushReload func(gen uint64, hash string)
(control.Reload, error)` returning `ReloadApplied | ReloadStale |
ReloadMismatch`; `Handler.PushState func(account string)
control.PushState` (`{Instance (filled in if empty), Generation, State,
Reason, LastDelivery}`; states `PushOff|PushStarting|PushDelivering|
PushQuiet|PushFailing|PushReauth`, reasons `ReauthReasons` /
`FailingReasons`). CLI side: `control.ReloadPush(path, gen, hash)
(Reload, error)` and `control.AskPushState(path, account)`, with
`ErrNotRunning` (never proof of no daemon: take daemon.lock) and
`ErrPushOff`.

**`internal/gmi`**: `Engine.Nudge(account) error`; `gmi.NudgeGap` (5s).

## As built: task 2

**The manager** (`internal/push`): `push.New(push.Options{Store, API,
Engine, Clock, Settle, OnChange, Logf})`, then `Start` (background:
takes `daemon.lock`, retrying each second while someone else holds it,
then reads `state.json`), `Stop` (cancel and join everything, then
release the lock; idempotent), `Reload` and `State` (the two control
handlers), `Woke`. main.go makes it before the control socket, wires
`srv.Push = pm.State` before the engine runs, starts it only once the
socket is up (no socket: never started, no `daemon.lock`, every account
off), and stops it after the socket closes and before the engine,
whose context is now its own. `push.WallNow` is the clock everywhere
(no monotonic reading), the API's too.

- `daemon.lock` is taken in server mode even with no `state.json` (it
  creates `push/`, 0700): a CLI must see the daemon to hand it the first
  generation. Until it's held, `push-reload` answers an error (pending).
- `Reload`: older generation `stale`; file not that generation and hash
  (or none) `mismatch`; the generation already applied with the same
  hash `ok` again once nothing cancelled is still running (a lost ack
  retried); a `client.json` that fails `state.CheckClient` is an error
  and changes nothing. A worker is replaced when its address or refresh
  token changes; every worker when the project, install, client ID or
  secret, owner sub or owner refresh changes. Joins are bounded by
  `JoinWait` (2s); past it the reload errors and the next one waits
  for the same workers.
- `State`: `Generation` is the applied one; an account with no worker
  (off, off-pending, absent, draining) is `off`. Health is per worker: a
  reload that keeps the worker keeps its `delivering`; a new credential
  starts again at `starting`.
- Wake: the epoch is bumped, requests in flight cancelled, tokens
  dropped; every answer is acted on (health, nudge, ack, a token, a
  reauth) only under the epoch's read lock with no wake since its
  request; requests are admitted only past the settle. The watch is
  renewed after every wake's settle (D5's "before pulling and renewing
  watches"), not only when due.
- Tokens: refreshed once less than `min(5 min, half its life)` remains;
  a refresh's waiters use its token however short. A 401 on pull, ack or
  watch drops the token and retries once at once.

**Health, beyond D5's letter:** `failing` also when no watch has been had
within `FailAfter` (3 min) of the worker's start or last wake: with no
`watchExp` yet, a watch that never came up read as `quiet`. Reasons: a
pull failure's code as `api-disabled`/`permission` (scope too)/
`org-policy`/`network` (unavailable too)/`unknown`, `network` when none
was recorded; a lapsed watch is `watch-expired` unless its last renewal
failed with an actionable code. `since` resets on a wake, so the 3
minutes count again from it. Live (2026-10-03), renewing a watch that
was still alive published nothing (C4's message is a new watch's), so a
restarted daemon heard nothing until real mail: `listening` (above)
covers the first 24h of a worker with no message, and `quiet` keeps its
meaning. googletest's fake still publishes on every watch.

**Logs**: `push <account>: <google op>: <code>` on a failure whose code
changed, `pulling again` / `watch renewed` on recovery, the reauth lines
naming `pneu account push <name>` and `pneu push init --reconsent`, and
the daemon.lock wait. Nothing else.

**For task 4** (client sanitizers, `StatusV2`, sync details):

- `AccountView.push` and `StatusAccount.push` are `web.PushView`:
  `{"state":…, "reason":…, "lastDelivery":…}`. The whole object is absent
  when push is off (`off` is never sent). `state` is one of
  `control.PushStates` minus `off`; `reason` is present only with
  `reauth` (one of `control.ReauthReasons`) or `failing` (one of
  `control.FailingReasons`); `lastDelivery` is RFC 3339 UTC to the
  second, present only once a message came. Hold each to that; anything
  else drops `push` for the account, never the event.
- The status file and SSE `account` follow a push state or reason
  change; a delivery alone doesn't rewrite the file (the sync it brings
  does).
- The polling note's "engine's real next delay" isn't exposed by task 2;
  it needs an accessor on `gmi.Engine` (its `delay`) and a field in the
  view if the page is to show it.

## As built: task 3 (the CLI)

What task 5 (INSTALL.md "Instant mail", AGENTS.md lines) needs. Code:
`cmd/pneu/push.go` (commands, transactions, hand-over), `pushwords.go`
(every Google failure in pneu's words), `callback.go` (the callback
listener, shared with the client relay), `accountstdin.go` (the remote
server half), `internal/remote` (verbs and schemas).

**Prerequisites** (D1): the push project with the **Gmail API** and the
**Cloud Pub/Sub API** enabled; its own **Desktop app** OAuth client
(External, In production), its JSON downloaded; the owner is an identity
that owns the project (Owner, or Pub/Sub Admin there). The CLI calls
`oauth2.googleapis.com`, `gmail.googleapis.com` and
`pubsub.googleapis.com`, and opens `accounts.google.com` in the browser.
Consent comes back to `localhost:8080`, like lieer's: not while a lieer
consent (`pneu account auth`, Reconnect) is waiting.

**Commands** (on a client each runs on the server over SSH, the consent
opening in the client's browser, exactly like `pneu account auth`):

```
pneu push init --project <id> --client-secret <file> [--replace] [--reconsent]
pneu push init [--reconsent]                 # again: project and client already set up
pneu account push <name> [--reconsent]
pneu account push <name> --off
```

- `push init`, first time: both flags required; the JSON must be a
  Desktop client (`"installed"`) whose `project_id` is `--project`. Owner
  consent ("Sign in as the identity that owns project P (the push owner)
  and allow Pub/Sub."), the ID token's sub pinned, a `topics.list` probe,
  then `push/client.json` (the three fields only) and `push/state.json`
  (generation 1, a fresh install id). Ends "Push is set up. Next, for each
  account: pneu account push <name>".
- `push init` again: the stored owner grant is reused when it still
  works ("the owner's stored grant (E) works; --reconsent asks again"),
  else a consent; a consent as anyone but the pinned owner is refused
  ("consent was given as a different Google account than the push owner,
  E, so nothing changed: …"). A different client or project needs
  `--replace`, and `--replace` needs every account off first ("--replace
  needs every account's push off first: pneu account push <name> --off for
  a, b"); `--replace` also accepts a new owner. A rotated secret for the
  same client is just `--client-secret <new file>`. Fix for an owner
  `reauth`: `pneu push init --reconsent` (or plain `push init`, which
  consents by itself when the grant is dead).
- `account push <name>`: the configured address must be a plain ASCII
  address held by no other pushed account. Order: owner refresh, mailbox
  grant (stored and working, else consent: "Sign in as A and allow pneu to
  see its mail's metadata (labels and headers, never bodies)."),
  getProfile must be the configured address ("the mailbox chosen at
  Google's consent isn't A, so nothing was set up: run this again and
  sign in as A"), then "topic pneu-<name>: created | already there",
  "Gmail may publish to it: granted | already granted", "subscription
  pneu-<name>: created | already there, as pneu makes it", "committed:
  <name> is on (generation N)", the hand-over, and up to 60s waiting:
  - "Instant mail is on for N: Gmail's watch delivered its first
    notification." (new subscription, same daemon process throughout);
  - "…its subscription is delivering. It existed before this run, so that
    shows messages arrive, not that this watch sent them." (reused
    subscription; similar wording if the daemon process changed);
  - "Instant mail is set up for N, but not proven: no notification reached
    pneu within 60s (it reports S[: reason]). Everything stays in place,
    and running pneu account push N again is safe." (exit 0);
  - reauth: an error naming the fix.
  Re-running is always safe; every provisioning step is idempotent.
- `account push <name> --off`: commits off-pending, waits for the
  daemon's ack, `users.stop`, removes the account, and prints:
  "Instant mail is off for N.", the `gcloud pubsub subscriptions delete
  pneu-N --project=P` and `gcloud pubsub topics delete pneu-N --project=P`
  commands (and the console's topic list), "pneu never revokes a grant. To
  revoke the push app's access to A, sign in as A at
  https://myaccount.google.com/permissions.", and, when A is the owner,
  "A is also the push owner: revoking either grant there ends both, and
  with it instant mail for every account."

**Hand-over words** (every command): ack → "The running pneu has it."
(init) or the wait; nothing answering and daemon.lock free → "pneu isn't
running; … when it starts."; a pneu answering without push → "The running
pneu runs no push sync; restart it …: systemctl --user restart pneu"; no
ack → an error ending "pending. Running … again is safe." (exit 1).

**Failure words** (`pushwords.go`, by code; never Google's text):
api-disabled → "the Cloud Pub/Sub API | Gmail API isn't enabled in project
P. Enable it at https://console.cloud.google.com/apis/library/<api
host>?project=P, then run this again"; org-policy on the IAM step → "an
organization policy (domain-restricted sharing) refused the grant to
gmail-api-push@system.gserviceaccount.com. Override
iam.allowedPolicyMemberDomains to Allow All on project P only
(https://console.cloud.google.com/iam-admin/orgpolicies/iam-allowedPolicyMemberDomains?project=P),
then run this again"; org-policy on a mailbox's consent → "A's Workspace
admin doesn't allow this app: in the Admin console, Security → Access and
data control → API controls, trust the push project's OAuth client for
Gmail metadata, then run this again"; permission on Pub/Sub → "the push
owner (E) lacks permission in project P: the owner must own the project (or
hold Pub/Sub Admin there)"; permission at the token endpoint → the client
was deleted or its secret changed (`push init --client-secret`); a
subscription not as pneu makes it → "…its <field> isn't what pneu makes:
delete it (gcloud pubsub subscriptions delete pneu-N --project=P) and run
this again"; another server's → "belongs to another pneu server (its
pneu-install label): one server per push project"; port 8080 taken → "a
consent is already waiting on localhost:8080 (lieer's or pneu's): finish
or close it, then run this again".

**Remote verbs** (fixed commands, stdin first-line schemas, all keys
required): `push` → `pneu account push --stdin` `{"name","reconsent"}`;
`push-off` → `pneu account push-off --stdin` `{"name"}` (no consent, no
relay); `push-init` → `pneu push init --stdin` `{"project","clientSecret",
"replace","reconsent"}`, the client JSON read and checked on the client
and sent as its three fields. The server checks the callback's state
against its own consent in constant time and exchanges only the code;
nothing listens on the server's 8080 for a remote consent. stdin's end
before the callback cancels the command (nothing is committed after it:
an ended context gets no push.lock); its end after is normal. The
client's relay gives up after 3 minutes without a consent URL, so a
remote run that needs no consent must finish within that (normally ~65s).

**Deviations from D2–D4**, all deliberate:
- `--off` holds push.lock from the off-pending commit to the removal,
  `users.stop` included (one refresh and one call, bounded by the API
  timeouts; the lock wait is 2 minutes): released around the stop, a
  concurrent `account push` could commit and start a watch the older stop
  ends.
- A dead mailbox grant at `--off`: stays off-pending (D4), with "The watch
  lapses by itself within 7 days, and nothing runs for N meanwhile; it
  stays off-pending until then. To clean up now: pneu account push N, then
  pneu account push N --off".
- `push init` is supported from a client (the JSON travels in the request,
  well inside the 16 KiB cap), and runs again without flags.
- Every commit re-reads under push.lock and refuses if what it read
  changed (setup, client.json, the owner's grant, the account's entry):
  "… changed while this ran; nothing was committed: run this again".
- An access token refused (401) is refreshed and the call made once more.
- The delivering proof counts only from the daemon instance `status`
  names before the hand-over, and only on a last message no older than
  the hand-over: a reload keeps a worker whose credential didn't change,
  health included, so a subscription deleted and made again under the
  same grants reads delivering on a message from before this run ("pneu
  kept the worker it already ran for it, …, not that this watch sent
  them"; found by the seam tests, `cmd/pneu/push_e2e_test.go`).

**AGENTS.md lines task 5 might add**: push's files and locks
(`$XDG_STATE_HOME/pneu/push/`: state.json and client.json written only by
the CLI under push.lock; daemon.lock the daemon's for life, taken by the
CLI only to decide "no daemon"); consent on the server binds 8080 lieer's
way and holds it; the remote verbs above; no Google text reaches any
output (pushwords.go is the only place failures become words).

## As built: task 4 (client and page)

- **Poll delay.** `gmi.Engine.PollDelay(account)` is `delay` as of now
  (the period, doubled per failure up to MaxBackoff, or the setup poll);
  `web.Syncer` gained it. The account view carries it as `pollEvery`
  (whole seconds, rounded up, absent when unknown); status.json doesn't.
- **Client** (`internal/client/events.go`): hello's accounts, `account`
  and `status` (hello's and the event's) decode `push` and `pollEvery`
  raw (an outer field shadowing the embedded `web` one), so a bad value
  of any type drops that field for that account, never the event.
  `cleanPush`: a string state from `control.PushStates` but `off`; a
  `reason` field present exactly with `reauth`/`failing` (present even
  as null or "" otherwise drops push), a string from that state's list;
  `lastDelivery`, when present, a string matching
  `YYYY-MM-DDTHH:MM:SSZ`, from 2000 to `DeliverySkew` (24h) past this
  machine's clock. `cleanPoll`: an integer in [1, 86400]. status.json v2
  carries `push` as cleaned.
- **Page** (`web/static/push.js`, `web/push.test.js`; app.js builds text
  nodes and `<code>` only): per account `Instant: delivering · last
  message 2m ago` / `listening · no message since pneu started` /
  `quiet · nothing in 24h` / `starting` / `off` / `failing — <words>`
  (reauth reads as failing too). Fixes: mailbox-reauth, watch-expired,
  unknown → `Run pneu account push <name>.`; owner-reauth → `Run pneu
  push init --reconsent.`; api-disabled, permission, org-policy →
  `pneu account push <name> prints the fix.`; network → none (it
  retries). On a client: "in a terminal on this machine (it runs on S
  over SSH and opens Google here if it needs to)", as task 3's remote
  verbs and `pneu account auth` work; not "on the server's terminal".
  The footer is the ready accounts' shortest `pollEvery` (else
  `data-every`); a ready account polling slower adds `checks every 4m
  while failing`. The status line never reads push.
- **Found on the way:** app.js keyed `accounts`, `busySince` and
  `asked` by name on plain objects, so an account named `constructor`
  (valid by `config.ValidName`) never joined the order; they have no
  prototype now. `TestNudgeRespectsBackoff` (task 1) read the failure
  count while the second run was still going; it waits for it now.
- **Tests:** `internal/client/push_test.go` (field rules with hostile
  values, two accounts through hello/status/account),
  `internal/web/push_test.go` `TestPollEvery`, `web/push.test.js`,
  `shell/status.test.js` (push in both versions, ignored by the widget;
  no QML change), and the hostile harness's `page-push-details`, which
  drives the shipping app.js's details with push states and hostile
  values.
