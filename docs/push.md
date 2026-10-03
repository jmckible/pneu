# Push sync: Gmail watch over Pub/Sub

Plan after two Codex design rounds (2026-10-03): first round C1–C19,
second round K1–K12. The 30-second poll (4942804) wasn't enough: the
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
user's (the **owner**; INSTALL.md suggests the personal account), with
its own Desktop OAuth client (External, In production, same branding
route as INSTALL.md step 4) used for nothing but push, the Gmail and
Pub/Sub APIs enabled (console, once), and every pushed account's topic
and subscription.

- **Revocation** (C6): revoking a grant revokes that user's scopes for
  the whole project. lieer's projects are separate, so nothing push does
  touches lieer. Within the push project the owner's two roles aren't
  separable (K5): if the owner is also a pushed mailbox, revoking either
  grant ends both. pneu never revokes (D4 `--off`), and says this
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
  publisher binding (C19). Prefer no organization; the error names the
  policy. A Workspace mailbox's admin may need to trust the push client
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

- **On the server:** pneu binds 127.0.0.1:8080 and [::1]:8080 with
  `CheckAuthPort`'s bind rule (no SO_REUSEADDR) and **holds** them until
  the callback (C9), with the relay's Host, method, path, size, time and
  one-shot checks, factored out of `relay.go` with the bind policy as a
  parameter (the client relay keeps its own). A bind failure is "a
  consent is already waiting (lieer's or pneu's); try again in a minute".
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
    (a message within 24h) → `quiet`.
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
  `org-policy`, `not-found`, `conflict`, `invalid-grant`, `quota`,
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
