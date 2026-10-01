# Client mode: one archive, many windows

One machine (the **server**, `dell`) holds the archive and runs lieer,
notmuch and the sync ticker. Other machines (**clients**) keep no mail. They
run a small pneu daemon that proxies to the server over the tailnet, and
they keep everything that belongs to the desk they sit at: the theme, the
bar widget, the status file, and `SUPER+M`.

Decided with the user (2026-09-30):

- The link is a **dedicated tailnet listener on the server** with
  per-client credentials. It is not an SSH tunnel and not `tailscale serve`.
- **The theme, bar widget and status file belong to the client.** Only mail
  comes from the server. The bar widget stays at parity; new-mail desktop
  notifications are a later project.
- **SUPER+M shows the last-known inbox at once, visibly updating.** The
  header's sync glyph becomes a status line that says what's going on.
- **Updates are a nudge plus a local rebuild.** Code on a client only comes
  from its own git checkout, never from the server.
- **Unreachable is an error state with directions.** Agent callouts
  (`omarchy agent prompt`) and updates are launched from the **bar widget's
  menu**, never from the page (R1).
- **Server-only work** (accounts, consent) runs on the server, reached over
  SSH from the client.

Reviewed adversarially (Codex, 2026-09-30) before any code, in two rounds.
R1… are the first round's findings, N1… the second's (made against the
first revision).

PLAN.md's "One person, one machine at a time" becomes "one person, one
archive". Everything else there stands, including the rule that the server
owns every `gmi` run.

## Shape

```text
 laptop (client)                                   dell (server)
 ─────────────────────────────                     ──────────────────────────────
 browser --app window                              browser --app window
   │ http://pneu.localhost:7317                      │ http://pneu.localhost:7317
   ▼  (existing Auth: Host, Origin,                   ▼  (existing Auth, unchanged)
      nonce → SameSite cookie)                     ┌──────────────────────────┐
 ┌─────────────────────────┐   mutual pinned TLS   │ core handler (mux)       │
 │ pneu client             │ ────────────────────▶ │  ▲ peer listener         │
 │  local: /open /theme.css│   tailnet IP only,    │  │ 100.x:7320, whois     │
 │   /events /client/*     │   whois lease         │ sync, notmuch, lieer     │
 │  proxy: allowlisted rest│                       └──────────────────────────┘
 │  status.json writer     │
 └─────────────────────────┘
   │  control socket (launch, agent, update)
 pneu open · bar widget (QML, reads status.json v2, owns the action menu)
```

One binary. `config.json` says which mode a machine is in. The same systemd
unit runs `pneu serve`, which starts either the server (loopback plus an
optional peer listener) or the client daemon. The config refuses to have
both `accounts` and `server`.

```json
// server (dell): today's config plus an optional peer block
{ "port": 7317, "accounts": [ ... ],
  "peer": { "port": 7320 } }

// client (laptop)
{ "port": 7317,
  "server": { "ssh": "dell", "node": "nxzXSZ2TfK11CNTRL", "port": 7320 } }
```

`server.ssh` is the SSH target the user typed at pairing. The server's
display name comes from its own hello and is never used as a target (R7).
Credentials don't go in `config.json`, because config dirs end up in
dotfiles repos. They live under `$XDG_STATE_HOME/pneu/peer/` with mode
0600, in a 0700 directory. Their owner and mode are checked on every load.

### The control socket

Both modes get a unix socket at `$XDG_RUNTIME_DIR/pneu/control`. The
directory is created 0700 and checked on every start: owned by this uid,
mode 0700, not a symlink (`Lstat`). Both ends check `SO_PEERCRED`. The
daemon accepts only its own uid, and the CLI refuses a socket answered by
a different uid (N8). This is the only way anything outside the browser
talks to the running daemon, and browser JavaScript has no way to reach a
unix socket. It admits any native process running as the user. That's the
boundary: it doesn't prove a click happened in QML, and it doesn't need to.

- `launch`, from `pneu open`. Queues a sync (R13, below). This is separate
  from the page's own `POST /sync` (`R` and focus), which stays a normal
  browser request.
- `peers-reload <generation> <hash>`, from `pneu peer add|remove` on the
  server. Answered only after that generation is live (R8).
- `agent <situation>` and `update`, from the bar widget via `pneu client
  agent|update` (R1).

## The link

### Credentials: mutual pinned TLS

Each side has its own self-signed key pair (ECDSA P-256, stdlib
`crypto/tls`). They exchange certificates once, when a client is paired, and
from then on each side accepts exactly the other's key: the SHA-256 of its
SPKI. No secret ever crosses the wire, and there's no bearer token to log,
replay or leak.

Why this and not a bearer token:

- **Server authentication matters as much as client authentication.** The
  client proxies HTML and JS into the `pneu.localhost` origin. A server
  impersonator can run script with pneu's origin on the laptop. With
  tailscaled down, packets to 100.x follow the default route, so on a
  hostile network someone else can answer at dell's address. Pinned TLS
  makes that a handshake failure.
- **A browser can't speak to the peer port.** A page can't present a client
  certificate it doesn't have, and it can't get past a pinned self-signed
  server certificate. That shuts out DNS rebinding and cross-site requests
  before any HTTP is parsed.

**Pins are checked in `VerifyConnection`, not `VerifyPeerCertificate`**
(R2). Go skips `VerifyPeerCertificate` on resumed sessions (TLS 1.3
tickets included), and `VerifyConnection` runs on every handshake. The
server also sets `SessionTicketsDisabled`, and the client sets no
`ClientSessionCache`. A handshake is cheap on a warm link, and "every
connection is fully verified" is easier to reason about than resumption
rules. (Go 1.27's `VerifyConnection` doc covers resumed connections, and
`SessionTicketsDisabled` turns off both issuing and accepting tickets.)

On the client, normal chain verification has to be off, or a
self-signed certificate fails before any callback runs. So
`InsecureSkipVerify: true` goes together with a `VerifyConnection` that
requires exactly one certificate whose SPKI matches the pin. That pairing
is the only verification, and it goes in one constructor with a test that
a different key is refused. `InsecureSkipVerify` never appears without it.

```go
// peer.go (sketch). Server side; the client's mirror checks one pin.
cfg := &tls.Config{
	Certificates:           []tls.Certificate{serverCert},
	ClientAuth:             tls.RequireAnyClientCert,
	SessionTicketsDisabled: true,
	MinVersion:             tls.VersionTLS13,
	NextProtos:             []string{"h2"},
	VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) != 1 {
			return errPeer
		}
		if s.peers.Lookup(spki(cs.PeerCertificates[0])) == nil { // current generation
			return errPeer
		}
		return nil
	},
}
```

**Authorization is re-checked on every request** against the current
peer generation, not only at the handshake (R2). The request context
carries the connection's SPKI hash; the guard looks it up fresh each time.
A removed peer's next request fails even if its connection somehow
survived.

**HTTP/2 is configured explicitly**, not assumed (R17). A transport with a
custom TLS config doesn't negotiate h2 unless told to. The client
builds one `http.Transport` just for the peer: `Protocols` set to HTTP/2
only, `Proxy: nil` (no environment proxies), a custom `DialContext` that
dials only the address resolved from tailscaled, and one idle connection.
A test checks the negotiated protocol.

### Tailscale as the second check

The server asks the local tailscaled's `whois` about the TCP remote
address. It uses the LocalAPI over the **fixed** socket path
`/var/run/tailscale/tailscaled.sock`, which must be root-owned, and never a
path from the environment. The predicate, exactly (R5):

- the address is one of the node's own `Addresses` (direct node addresses
  only: no subnet-routed, exit-node or 4via6 sources);
- `Node.StableID` equals the one recorded at pairing;
- `Node.Tags` is empty (a tagged node's `User` is only its creator);
- `Node.Sharer` is zero (not shared in from another tailnet);
- `UserProfile.ID` is non-zero and equals the server's own user
  (`Self.UserID` from LocalAPI status).

**Authorization is a lease on every live connection, on both sides**
(R5, N9). Each connection carries an expiry. A sweep every 10s renews
every connection whose lease has less than 20s left, whatever it's
carrying (SSE, a long attachment download, nothing) and whether or not
new requests arrive. Each whois call has a 2s deadline. A connection whose
lease reaches expiry without a successful renewal is closed, with every
stream on it. That puts a hard bound on stale access: at most 60s after
the last good check. The client runs the same lease on its own pooled
connection against the server's node. "Fail closed" covers the whole
lifetime, not just the handshake.

The client runs the mirror check before dialing. It resolves the server's
current address from its own tailscaled by `StableID` (not MagicDNS),
refuses to dial when `BackendState` isn't `Running`, and whois-checks the
address it's about to dial.

What this adds on top of TLS: a copied client key alone is useless from
another device, and a tagged or shared-in node can't use a key even if it
has one. **Out of scope:** a key copied *together with* the laptop's
Tailscale node identity (a full disk image), or traffic relayed through the
paired laptop. That is a compromised laptop (T7).

Tailnet ACLs are recommended in INSTALL.md (a grant so only your own
devices reach `dell:7320`), but nothing depends on them.

### The listener

- It binds **only dell's direct tailnet addresses** (v4 and v6, from
  LocalAPI), never `0.0.0.0`. The daemon reconciles every 10s: it binds
  addresses that have appeared and closes listeners on addresses that are
  gone, or all of them when the backend isn't `Running` (R17). The
  loopback listener never waits for any of this.
- Its handler is the same `mux` behind a different guard: `PeerAuth`
  instead of `Auth.Middleware`.

```go
core := s.routes()                        // today's mux, minus /open
s.local = s.Auth.Middleware(withOpen(core))
s.peer  = s.PeerAuth(core)                // pin + lease + generation, per request
```

- The peer guard **doesn't check cookies, the Origin header or the nonce.**
  The client proxy has already applied its own Auth to the browser
  request. Upstream the only credential is the TLS peer. Host must still
  be one of the server's tailnet addresses.
- `/open` doesn't exist on the peer listener.
- `Page.Origin` (the page's `data-origin`) comes from the peer record
  (`http://pneu.localhost:<port>`, validated to exactly that shape at
  pairing), never from a request header.
- Logging names the client (`peer macbook-1`), never certificate bytes.

### Pairing

Pairing runs on the client, over SSH, and asks nothing of the browser:

```sh
pneu client pair dell
```

1. The client makes its key pair and reads its own `StableID` from
   tailscaled.
2. It runs **one fixed remote command** with every SSH capability it
   doesn't need turned off (R7):

   ```sh
   ssh -T -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes \
       -o ControlPath=none -o PermitLocalCommand=no \
       -- dell 'pneu peer add --stdin'
   ```

   Parameters go on **stdin as one bounded JSON object** (name, node,
   origin, certificate PEM; 16 KiB cap), never as arguments: SSH joins its
   arguments with spaces and hands them to the remote shell, so argument
   boundaries don't survive. The SSH target is passed to `exec.Command` as
   its own argument after `--`. Host-key checking is the user's normal
   setting. An unknown host is the user's TOFU decision, which `pair`
   points out and doesn't paper over.
3. `pneu peer add` on dell takes an exclusive `flock` on
   `peers.lock`, a file that's never replaced. Locking `peers.json`
   itself would be broken by the rename below (N8). Holding the lock, it
   reads `peers.json`, refuses a name, node or SPKI already present (a
   re-pair is `peer remove` then `peer add`), and writes the file
   atomically (temp file + rename, 0600) with a new **generation** and
   the content's hash. It then asks the running server over the control
   socket to load that generation, and waits for an acknowledgment that
   names both generation and hash, still holding the lock (R8). If the
   daemon isn't running, the file is written and the command says the
   change applies at the next start. If the acknowledgment times out
   (5s), it says the change is **pending** and never "done". It prints bounded JSON on stdout: dell's
   certificate, dell's `StableID`, peer port, protocol version and name.
   Diagnostics go to stderr only.
4. The client parses stdout strictly (size cap, known fields, PEM
   parses as one certificate), pins dell's key, and writes its config.

`pneu peer list | remove <name>` on dell. `remove` goes through the same
lock, generation and acknowledgment. The server acknowledges only after
the new generation is live **and** every connection from that peer is
closed. Registering a connection and removing a peer share a lock, so a
handshake can't land between the check and the close (R2, R8).

## The client daemon

### Local auth is unchanged

The client serves `pneu.localhost:<port>` with the existing `Auth` exactly
as the server does today: its own install token, its own nonce for `pneu
open`, the Host and Origin checks, and `SameSite=Strict`. Every rule in
`auth.go` still applies, because the threat is the same: the laptop's
browser is also its daily browser.

### Proxy rules

`httputil.ReverseProxy` with `Rewrite` (not `Director`) and the peer
transport above.

**Routing.** These are answered locally: `/open`, `/theme.css`, `/events`,
`/client/*` (including `/client/static/`: the error page's own assets,
embedded in the client binary, so it renders with the link down; R17).
Everything else goes upstream only if it matches the **route table**.

The route table is shared Go data, compiled into both binaries. Each
entry is a method, a `ServeMux` pattern, and the route's response
contract (N1):

```go
type Route struct {
	Method, Pattern string   // "GET", "/part/{account}/{msgid}/{n}"
	Policies        []Policy // classes this route may answer with
	Types           []string // allowed parsed media types, per policy
	Disposition     DispRule // none | inline | attachment (filename re-encoded)
	Dest            DestRule // Sec-Fetch-Dest constraints (SVG: image only)
	Cache           CacheRule
}
```

The server registers its handlers from the table, and the client builds
a `ServeMux` from the same table to match requests. So matching is the
same code on both sides (escaped segments, `{$}`, GET-implies-HEAD), not a
second list of strings kept in sync. An unmatched request gets a 404
locally. The client never forwards arbitrary paths.

**Requests going up** (R3):

- `Rewrite` sets a fixed upstream scheme and authority. Every hop-by-hop
  header goes, plus any header named in `Connection`, `Cookie`,
  `Authorization`, `Origin`, `Referer`, `Forwarded`, every `X-Forwarded-*`
  and `X-Real-IP`, `Upgrade`, `TE` and trailers.
- A request carrying `Service-Worker: script` is refused locally (R4).
- Method must be the route's method. `CONNECT` and upgrades are refused.

**Responses coming down** (R3, R6). Go's `ReverseProxy` *appends*
upstream headers. It forwards 1xx responses through a trace callback it
installs *after* `Rewrite`, and `httptrace` composes callbacks rather
than replacing them, so adding a no-op hook doesn't suppress it (N5). The
existing `cspWriter` would take a forwarded 103 as its one header write.
So the proxy writes into a **final-response writer** that owns the
boundary:

- `WriteHeader` with a 1xx status is swallowed: nothing reaches the
  browser, and the header map is reset. The first status ≥ 200 is the only
  header write that goes through. At that moment the writer clears the
  map and installs the complete authoritative set below. Trailers are
  never declared, and anything written to the trailer keys is dropped.
  Tests run 103→200, 100→200, duplicate headers, `Connection`-nominated
  headers and late trailers through the composed middleware, not the
  proxy alone.
- **The client decides the policy; the server only nominates it** (N1).
  The server sends `Pneu-Policy`. The client accepts it only if it's one
  of the route's allowed classes **and** the parsed `Content-Type` is
  allowed for that class on that route **and** `Sec-Fetch-Dest` fits the
  route's rule. Otherwise the response is replaced by a local 502. So
  `part-pdf` (the `SAMEORIGIN` / `frame-ancestors 'self'` exception)
  exists only on part routes with a parsed `application/pdf`. `app` exists
  only on page routes with `text/html`. An SVG passes only as an image
  destination. `compose` (the global set with `Referrer-Policy:
  same-origin`, which `compose.go` needs so Chromium sends a real Origin
  on the form POST; N4) exists only on `/compose`, `/reply/…` and
  `/send`'s rendered errors.
- Security headers come from **the same functions the server uses**
  (shared code in `internal/web`), keyed by the accepted class. The
  server's loopback path calls them too, so the two can't drift.
- **Kept from upstream, after validation:** exactly one `Content-Type`
  (parsed, then re-emitted); `Content-Length`; `Content-Range` and
  `Accept-Ranges` on part routes; `Content-Disposition`, **parsed and
  rebuilt** (type from the route rule, filename re-encoded the way
  `rfc5987` does today), never forwarded verbatim; `Location` (below).
  Everything else is dropped, including `Cache-Control`, `ETag` and
  `Last-Modified` (N2).
- **Caching is local policy** (N2). Pages, mail, parts and JSON get
  `Cache-Control: no-store`, as today. `/static/` gets `no-cache`: the
  browser revalidates on every load, and the client answers the
  revalidation by forwarding it and passing back only a 304 or a fresh
  200. It never passes upstream freshness lifetimes. A compromised dell
  can't pin a hostile `app.js` in the laptop's cache beyond its own
  repair: the next load after the repair gets the repaired file.
- **`Location`** (N4) is resolved against the local origin. It's allowed if
  the result stays on `pneu.localhost:<port>`, with no scheme, no
  authority, no backslash and no control character in the original value.
  The one off-origin exception is `/gmail/{account}/{thread}`, whose
  redirect must pass a shared `validGmailURL`. The Gmail message ID
  comes from dell's file lookup, so the client can't rebuild the URL
  itself. It checks the exact shape `gmailURL` produces instead: `https`,
  host exactly `mail.google.com`, the fixed path, `authuser` equal to the
  account's address, a hex message ID, and nothing else. A redirect that
  doesn't match becomes a 502.
- `Set-Cookie` never passes. (Script from a compromised dell can still set
  non-HttpOnly cookies on the origin itself. That can't touch the session
  cookie, which is HttpOnly, and it's covered by T6.)

**Limits and timeouts** (R14, R17):

- A response header timeout of 10s.
- Mail bodies stream without a size cap. A **per-response** idle timer
  cancels that one stream after 60s without a byte. It isn't a
  connection deadline, which would take down every stream on the shared
  connection, SSE included.
- Everything that isn't mail (hello, events, status, pairing output,
  diagnostics) has its own caps: size, nesting depth, element counts and
  string lengths.

### Events

The client holds **one** upstream SSE stream and fans it out to its own
`Hub`.

- **Handshake, then allowlist** (R14, N10). The first event on an
  upstream stream must be `hello`; that's the one place `hello` is
  accepted. After it, only `syncing`, `sync`, `account`, `auth`, `status`
  and `view` are accepted. Each event is at most 64 KiB and is decoded
  into its typed struct before being re-encoded for local browsers.
  Names the client generates itself (`link`, `theme`, `hello`) are never
  accepted after the handshake. An event naming an account that isn't in
  the `hello` account set is dropped (N14).
- **Snapshot on subscribe, ordered** (R12, N10). The server **subscribes
  the connection to the `Hub` first** (`sse.go` already does this before
  its preamble). Then it takes the snapshot under the same lock that bumps
  the view generation, and sends `hello` with `(epoch, generation)`,
  protocol, account set, status doc and running syncs. Events queued
  behind it carry their generation, and anything at or below the
  snapshot's generation is dropped. (Server mode, built: `/events`
  subscribes inside that lock, and inside `account`'s, so nothing at or
  below the snapshot is ever queued; the client's upstream reader still
  drops by generation, since it can't hold dell's locks.) `epoch` is random per server start, so
  a restart can't reuse a generation an old page is holding. The client's
  local `/events` does the same for each browser connection, from its own
  state.
- **Pages are labeled conservatively** (N10). A page renders with the
  `(epoch, generation)` read **before** its notmuch query, never after. A
  write that races the query makes the page look older than it is, so it
  reloads once too often, never once too few. `app.js` compares its label
  with each `hello` and reloads the list when they differ.
- **Liveness.** The server's `Hub` already sends a heartbeat comment
  every 25s. The client calls the link down after 60s with nothing. On
  `launch` and on window focus, the client also sends a 3s hello probe,
  because a laptop that just woke up doesn't have 60s to wait (R12).

### Theme

`/theme.css` and `WatchTheme` read the laptop's theme files, the same code
as today. Upstream `theme` events never pass the allowlist.

### Status file and the bar

The server broadcasts `status` over SSE whenever its status file is
rewritten. The client decodes it into the typed `statusDoc` with limits
(≤ 16 accounts, ≤ 5 senders, strings ≤ 128 runes, control characters
stripped). Then it writes the laptop's `status.json`, which the client
owns completely (R14):

```json
{ "version": 2, "updated": "...", "running": true, "unread": 3,
  "senders": [...], "accounts": [...],
  "server": { "name": "dell", "link": "up", "linkSince": "...",
              "statusAt": "...", "reason": null, "update": null } }
```

- `updated` and `running` describe **this machine's daemon**. `statusAt`
  is when the last valid status arrived from dell. The widget treats the
  counts as stale when `statusAt` is old or `link` isn't `up`, even though
  `updated` keeps ticking (R12).
- `link`, `reason` and `update` are computed locally from **local reason
  codes** (an enum). No text from the server goes into them. Account
  names and sender names *are* dell's text, bounded and plain (N14).
- **Writes are coalesced** (N14): at most one `status.json` write per
  second, the latest status wins, and at most one decoded status is held
  pending. A dell flooding valid events costs a parse each, not a disk
  write each.
- The widget must render every one of these strings as `Text.PlainText`
  (R14). That's the case today in the installed bar's tooltip; this makes
  it a stated requirement, with a test fixture holding markup in a
  sender name.
- The widget accepts version 1 or 2. `server` is absent on the server
  itself.

### The action menu (bar widget)

The bar widget's click menu gets the actions that have effects outside the
browser. They show up only when relevant (R1):

- **Fix with agent**, when `link` isn't `up` or a protocol mismatch
  exists. Runs `pneu client agent`, which asks the daemon over the
  control socket.
- **Update pneu**, when `update` is set. Runs `pneu update` in a floating
  terminal.
- **Update dell**, when dell is the older side. Runs `pneu client agent
  update-server`.
- **Reset window data** (R4, below).

The agent prompt is a fixed template per situation. Its only
interpolated values are the local reason code, this binary's own
revision, the SSH target from local config, and the remote revision **if
it matches `^[0-9a-f]{40}$`** (otherwise the word "unknown"). The prompt
says outright that output from dell over SSH is untrusted data.

The page can't launch anything. `/client/*` on the local HTTP side is
read-only link state and `retry`. The error page and the status line
explain the situation and name the menu item.

## Freshness on SUPER+M

### The path

`SUPER+M` → `pneu open`. That now **first sends `launch` over the control
socket** (R13), and only then runs `omarchy-launch-or-focus-webapp` with
the nonce URL. `launch` queues the sync however the window then opens:
a fresh nonce, a cookie that's already there (today's `/open` fast path
skips `OnLaunch`, so reopening a closed window never synced), or a focus
of an existing window. In server mode `launch` calls `syncAll()`. In client
mode it sends `POST /sync` upstream and a hello probe. Nothing a browser can
send triggers it. The `OnLaunch` hook on `/open` goes away.

**This fixes today's server mode too.**

The page then renders dell's database as of its last sync. What's on
screen is the source; what may be behind is the source relative to Gmail. Two
minutes is the healthy polling target. During failures or a first pull it
isn't a freshness bound (R13), and the status line says so.

The warm connection keeps the render at one tailnet round trip: the
client holds the SSE stream open all the time, so the handshake and path
setup have already happened.

### The status line (both modes)

The braille glyph at the header's far right is replaced by a short status
line:

| State | Shows | When |
|---|---|---|
| checking | spinner + `Checking…` in accent | any sync queued or running |
| fresh | `Updated just now` → `Updated 3m ago`, muted | idle; age of the oldest account's last successful sync |
| stale | `Updated 14m ago` in `--accent-hot` | idle and older than 3 ticks |
| account error | `work: sync failing` in `--accent-hot` | failures ≥ the bar's threshold |
| link down | `Can't reach dell · retrying` in `--accent-hot` | client only |
| update | `Update available`, muted, appended | version nudge pending |

- **First paint is right** (R13). `data-accounts` gains `lastSync` and
  `queued/running` per account. A `launch` marks every account queued
  before its request returns, so a page rendered right after SUPER+M
  already says `Checking…`.
- Launch and focus syncs are **no longer quiet**. That's the moment the
  user is looking.
- Clicking it, or `?`, opens the details: each account's last sync,
  failures, link state, both revisions, and which bar-menu action fixes
  what.
- The age updates every 30s, and on focus.

### Changes from other windows (R11)

Today a label-only push deliberately doesn't broadcast `sync`, so the
window that archived isn't reloaded under itself. With two windows (dell
and the laptop), the other one would never see the archive. The server
keeps a **view generation**, bumped on every successful tag write, undo,
mark-read and changing sync, and broadcasts `view {epoch, gen, from, threads}`. `threads` lists the
`(account, thread)` pairs the write touched. `from` is an ID the page
sends with each write (`X-Pneu-Window`, random per page load).

- **`from` is only a hint for skipping a refresh; it never authorizes
  anything** (N11). A compromised dell can spoof it; a hostile page on
  another origin can't set it through the mutation guard. A window skips
  its own `view` only when it has **already applied** that write's
  successful response. If the write is still pending or its outcome is
  unknown, the event is how the window learns the truth, so it reconciles.
- Other windows reload the list in place without moving the cursor (the
  existing `loadList` path). An open thread whose `(account, thread)` is
  in `threads` gets refetched, or marked `changed elsewhere` if it's in
  the middle of an action. `loadList` alone would leave it stale.
- **Undo conflicts are per thread, not per generation.** An undo entry
  records the thread's last `gen` it saw. If a `view` arrives naming that
  thread with another window's `from`, the toast becomes `changed
  elsewhere · z undoes yours anyway`. It's still last-writer-wins, but the
  toast says so.

## Unreachable, mismatch, unknown outcomes

When the link isn't up, page requests get a local page (with
`/client/static/` assets) that says what the daemon knows:

| Reason | Detected by | Says | Bar menu |
|---|---|---|---|
| `tailscale-down` | LocalAPI unreachable or `BackendState` ≠ Running | Tailscale is off on this machine | Fix with agent |
| `node-offline` | the server's peer is `Online: false` | dell is offline (last seen …) | Fix with agent |
| `refused` / timeout | dial | dell is up but pneu isn't answering | Fix with agent |
| `pin-mismatch` | TLS verify | dell's identity changed; not connecting | Fix with agent (re-pair steps). **Never** a "trust anyway" |
| `not-paired` | TLS alert | dell doesn't know this laptop | `pneu client pair dell` |
| `protocol` | hello | client and server protocols differ | Update pneu / Update dell |

- The daemon retries with backoff (1s → 30s cap), and right away on
  `launch`, focus, or `R`. `pin-mismatch` is never retried in the
  background.
- An open page that loses the link keeps its content and switches the
  status line.

**Mutations over a failing link** (R10). The client can tell two cases
apart, and the toast says which:

- **Not sent**: the failure provably happened before dispatch: no
  connection could be made (the link was already down, the dial failed,
  or the TLS handshake failed) before any request byte was written. `Not
  sent: can't reach dell.` Safe to press again.
- **Outcome unknown**: everything else, **including an error while
  writing the request** (N6). Bytes may have reached dell before the
  error surfaced.
  `dell didn't answer; checking when it's back.` The tag write may well
  have landed (`tag.go` finishes writes on purpose after the request is
  cancelled). On reconnect the `hello` generation reloads the list, which
  shows the truth. No mutation is ever retried automatically.
- **Send is reserved before it's sent** (N7). Today `compose.go`
  claims the draft ID in memory before sending, and it tells Gmail
  accepting the message apart from the local copy failing. Both carry
  over, made durable:
  1. `POST /send` carries `(account, draft ID, payload hash)`. Under a
     per-draft lock the server writes a **reservation** to
     `$XDG_STATE_HOME/pneu/sends/` (fsync, 0600) **before** handing
     anything to lieer.
  2. It sends, then records the terminal result (accepted, accepted with
     the local copy failed, rejected) over the reservation.
  3. A repeat with the same ID and hash gets the recorded result. The
     same ID with a different hash is refused.
  4. A reservation with no result (a crash between Gmail accepting and
     the record being written) is **unknown**, never retryable. The UI
     says `may have been sent: check Sent` and offers no resend. Local
     persistence can't make Gmail's acceptance and the local record
     atomic; this makes the gap visible instead of hiding it in a
     duplicate.
  5. The draft ID survives losing the window: it moves from
     `sessionStorage` to the draft's saved state, so a reopened draft
     keeps its ID. Records are kept 30 days. That is the stated limit of
     the guarantee, and an unsent draft older than that gets a new ID. Unsubscribe already binds
  preview to execution (actions.md R3). Tags are add/remove, and a reload
  shows their real state.
- There's no offline queue. The undo model assumes the write happened, and
  a queue is a different product.

## Versions and updates

### What skews

HTML, JS, CSS and templates all come from **dell's** binary, so the UI is
always the server's version. The client's version covers the proxy, the
link protocol, the status file, the theme watcher, the bar widget, and
`pneu open`. So skew mostly needs a nudge.

- **Protocol** (an integer, in `hello` and `Pneu-Protocol`): it changes only
  when the link contract does (the route table, event shapes, policy
  classes, the status doc). A mismatch is the error state above.
- **Build**: `debug.ReadBuildInfo()` `vcs.revision` and `vcs.modified`.
  The same revision with no modifications means the same build. Anything
  else is **different**. Which side is older is worked out only by `git
  merge-base --is-ancestor` in the client's checkout after a fetch. When
  that can't answer (diverged, unknown revision, dirty build, missing
  metadata), the nudge says "different builds" and doesn't claim which is
  older. `vcs.time` is never used (R16).

### `pneu update`

Runs on either kind of machine, from the checkout and the **remote and
branch recorded at install** (`"source"` in config) (R15):

1. Take an exclusive lock (`$XDG_RUNTIME_DIR/pneu/update.lock`).
2. Refuse if the tree is dirty or `HEAD` isn't the recorded branch.
3. `git fetch <remote> <branch>`, resolve **one** target revision, and
   require that `HEAD` is its ancestor. That's a history constraint (no
   rewrites under you), not a sign the code is trustworthy. The trust is
   the same as at install: the user's own repo.
4. Show the incoming log and ask for confirmation in the terminal.
5. **Build before touching anything live:** `git worktree add` the
   target in a temp dir, `go build` into `~/.local/bin/.pneu.new` (same
   filesystem, so the rename is atomic), and run its `version` as a
   smoke check (after trust is settled, not as trust).
6. **Record the deployment** in `$XDG_STATE_HOME/pneu/update.json`: old
   and new revision, and old and new binary SHA-256. An interrupted
   update is found and finished or rolled back on the next `pneu
   update` (N13).
7. Deploy: **recheck** that the tree is clean and `HEAD` hasn't moved (an
   editor or a manual git command doesn't take the lock). Copy the live
   binary to `pneu.prev`, rename `.pneu.new` in, fast-forward the
   checkout (the widget loads from it), and restart the service.
8. **Verify readiness, not just liveness** (N13): the control socket's
   `status` must report the **new** revision and a fresh instance ID,
   and a local `GET /client/link` (client) or `/status` (server) over
   loopback must answer. Timeout: 20s.
9. **Rollback is a transaction too:** stop the failed instance, rename
   `pneu.prev` back, reset the checkout to the old revision **only if**
   it's still clean at the new revision (otherwise leave it and say so),
   start the service, and verify the old revision the same way. Say
   exactly which state things ended up in.
10. Remind the user that the widget changes take effect on the next shell
   restart. `pneu update` doesn't restart the shell.

## Server work from a client

A client with no archive refers server work to dell over SSH, with the
same fixed options as pairing (R7):

- `pneu account add|auth|status|…` on a client prints what it will run,
  then runs `ssh -T … -- dell 'pneu account <verb> --stdin'`, where
  `<verb>` comes from a fixed enum and the arguments go as JSON on stdin.
  The arguments are never joined into the remote command line. No PTY:
  the remote side speaks line-delimited JSON events on stdout (prompts,
  progress, the consent URL, the result), and the client renders them. A
  PTY would mix echo and framing into that channel.
- **Consent over a forward** (R9). lieer's consent flow waits for Google on
  dell's `localhost:8080`, but the Google page opens on the laptop. `pneu
  account add|auth` from a client:
  1. checks that the laptop's 8080 is free by **binding** `127.0.0.1:8080`
     and `[::1]:8080` explicitly, then releasing them. Today's
     `CheckAuthPort` does a single `localhost` listen, which isn't the
     same thing (N12).
  2. runs SSH with its **own** option set. Pairing's
     `ClearAllForwardings=yes` also clears command-line `-L`s. That was
     checked against OpenSSH 10.5 with `ssh -G`, from both the command
     line and a `-F` file (N12). So consent uses `ClearAllForwardings=no`,
     and **first** resolves the target's config with `ssh -G <target>`:
     if any `LocalForward`, `RemoteForward` or `DynamicForward` is
     configured for it, consent refuses and names the lines, rather than
     inheriting forwards the user didn't intend for this session. Then:
     `-T -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=no
     -o ExitOnForwardFailure=yes -o ControlPath=none -L
     127.0.0.1:8080:127.0.0.1:8080 -L [::1]:8080:127.0.0.1:8080`, bound
     explicitly to loopback so `GatewayPorts` doesn't apply. Both binds
     must succeed (`ExitOnForwardFailure`) before the URL is opened.
  3. passes `consentOpen: "print"` in the stdin parameters, so dell sends
     the URL as an event instead of running `xdg-open` on dell's screen
     (today `consentOpener` opens it on whichever machine runs the
     command);
  4. **validates the URL locally** with the same Google-only rule
     `consentOpener` uses, then opens it in the laptop's browser.

  The forward lasts only as long as that SSH session. While it's up, a
  page in the laptop's browser can reach lieer's callback server. At worst
  a stray request uses up the one-shot callback (it handles one request)
  and consent has to be run again. The OAuth `state` check still stands
  between a stolen code and a usable credential.
- **Reauth from the UI** (`POST /accounts/{a}/reauth`) is refused on the
  peer listener with `409 reauth-on-server`. The UI shows `pneu account
  auth <name>`, and the bar menu's **Fix with agent** covers it.

## Service workers and a poisoned origin (R4)

`AppCSP` allows same-origin script and doesn't restrict workers. On
either machine, script that runs in the pneu origin could register a
root-scoped service worker. A worker outlives the page and the fix,
intercepts future requests, and could replace the local error page. In
client mode dell's HTML runs in the laptop's origin, so a compromised dell
could leave one behind.

- `AppCSP` gains `worker-src 'none'` (both modes; the hostile corpus
  re-runs against it).
- The client refuses any request with `Service-Worker: script`, and never
  serves `/sw.js`-style paths because they aren't in the route table.
- **Recovery can't depend on the network path** (N3). A root-scoped
  worker can answer navigations itself, so a `Clear-Site-Data` response
  from the daemon may never reach the browser, and pages already open keep
  running whatever code they loaded. The bar menu's **Reset window data**
  therefore:
  1. closes every pneu window (the `--app` window class, through
     `hyprctl`), so no page is left holding code in memory;
  2. clears the origin's data **through the browser**. With the window
     closed, the daemon launches the browser profile's
     `chrome://settings/content/all?searchSubpermissions=pneu.localhost`
     page and tells the user the one click to make. A browser-owned clear
     is the only reliable mechanism the Clear-Site-Data spec names. There
     is no scriptable "delete site data for origin" in Chromium without
     DevTools.
  3. as a best effort as well, answers the next navigation that does reach
     it with `Clear-Site-Data: "cache", "storage"`, and serves any
     worker-update check for a script URL with a 404 so a registered
     worker is dropped;
  4. reopens pneu and checks `navigator.serviceWorker.getRegistrations()`
     comes back empty, reporting through the status line.

  The menu item says up front that it deletes locally saved compose
  drafts (`"storage"`). The cookie is reissued at the next nonce launch.

## Threat model

- **T1: hostile page on the laptop → local daemon.** Existing Auth. No
  change. `/client/*` is read-only apart from `retry`.
- **T2: hostile page on the laptop → dell's peer port.** The TLS
  handshake fails. No HTTP is parsed.
- **T3: impersonating dell** (tailscaled down, hostile LAN answering on
  100.x, MagicDNS spoofing). The pin fails in `VerifyConnection`. The
  client also refuses to dial unless tailscaled is `Running` and whois
  matches.
- **T4: another tailnet user, tagged or shared-in node.** The whois
  predicate, leased and rechecked. ACLs are recommended, not relied on.
- **T5: stolen client key.** Bound to its node's `StableID`, revoked by
  `peer remove` (acknowledged, connections closed). A key plus the node's
  Tailscale identity is T7.
- **T6: compromised dell.** It has the mail already. It controls the HTML
  in the laptop's origin, which means it runs arbitrary same-origin script
  there. **It can't:** launch anything on the laptop (R1); put text into
  agent prompts (local enums, validated revisions); set status fields
  other than bounded, plain account and sender names (N14); pick a
  security policy the route doesn't allow for that content type (N1); pin
  executable content in the browser cache past its own repair (N2); set
  the session cookie; register a service worker (R4); or supply code
  (`pneu update` fetches from the recorded remote). **It can:** show false
  mail, set non-HttpOnly cookies, keep the daemon parsing valid events up
  to the stream's work budget (256 KiB and 50 events a second sustained,
  bursts of 2 MiB and 200; past it the stream is closed and reopened with
  backoff; status writes coalesced to one a second),
  and stream unbounded bodies into the browser. The browser tab's memory
  is dell's to exhaust, just as it is when dell's own app JS runs there
  (N14). The daemon's memory is bounded: one upstream connection,
  streamed bodies, capped events.
- **T7: compromised laptop.** Full mail access until `peer remove`.
  Accepted, the same as a compromised dell.
- **T8: trust in headers.** The only upstream identity is the TLS peer,
  re-checked per request. The rendering origin comes from the peer
  record.
- **T9: pairing.** SSH host-key trust (the user's TOFU for new hosts)
  vouches for the exchange. Fixed remote command, JSON on stdin, no
  forwarding, no agent. Only public certificates cross.
- **T10: logs.** Names and node IDs only.

## Gotchas

- **Tailnet addresses change and appear late.** The listener reconciles
  with LocalAPI; it doesn't bind once at boot.
- **Sleep.** A dead TCP connection looks alive until something is written
  to it. The 25s heartbeat with a 60s cutoff covers the background case;
  the hello probe on `launch` and focus covers the moment you look.
- **One shared HTTP/2 connection.** Idle limits are per stream. The fan-out
  drops a slow browser rather than backing up the upstream read (the
  `Hub` already drops slow clients).
- **`data-origin` must be the client's**, or the local Origin check
  refuses every POST from the laptop. That's why the origin is stored at
  pairing.
- **Unsubscribe runs on dell.** The DKIM checks and the one-click POST
  come from dell's network. That's fine: it's the same Gmail identity.
- **Same port on both machines,** so `pneu open`, the desktop entry and
  the window class stay the same.
- **The route table is shared code.** A server route the client doesn't
  know about is a 404 on the laptop until the client is updated. That's
  what the protocol integer is for: adding a route bumps it.

## Install split

INSTALL.md forks after the audit and the build:

- **Shared:** Briefing, Audit, packages (the client list drops `lieer` and
  `notmuch`; `install/packages` takes `server|client`), build, desktop
  entry, theme hook, bar widget.
- **Server path:** today's steps 3 onward, plus an optional *Let other
  machines in* step: add `peer` to the config, restart, and point to the
  ACL recommendation.
- **Client path:** confirm Tailscale is up on both machines and that `ssh
  dell` works (with host-key verification), run `pneu client pair dell`,
  then service, `pneu open`. No Google steps at all.

New audit items: the peer listener binds only tailnet addresses (`ss
-ltnp`); a connection with no certificate fails the handshake (`curl -k
https://<dell tailnet ip>:7320/`); and the tests cover a wrong-node peer,
a removed peer on a live connection, resumption, the 103→200 header path,
and markup in a sender name reaching the widget.

## Build order

Each step works on its own. Codex review happens at the steps marked ◆.

1. **Launch and freshness (server mode):** the control socket, `pneu open
   → launch`, `data-accounts` with `lastSync`/queued, and the status line.
   It fixes the reopen-without-sync bug on dell today.
2. **View generation:** `view` events and `X-Pneu-Window`, plus the
   `hello` snapshot on `/events`. Needed as soon as two windows exist, and
   useful on one machine with a stale tab.
3. **Hardening shared by both modes:** `worker-src 'none'` in `AppCSP`,
   policy classes as shared functions, the route table as data, send
   idempotency. ◆ (hostile corpus re-run)
4. **Peer listener:** TLS with `VerifyConnection`, whois lease, the
   generation-locked `peers.json`, `pneu peer add|list|remove`, hello and
   protocol, listener reconciliation. ◆
5. **Client daemon:** pairing, proxy (route allowlist, header enforcement,
   per-stream limits), event allowlist and snapshot, local theme, status
   v2, error page, `Reset window data`. ◆
6. **Bar widget v2:** stale-by-`statusAt`, the action menu,
   `Text.PlainText` fixtures, the INSTALL.md split, and the audit items.
7. **Versions and updates:** build info, ancestry nudge, `pneu update`,
   agent callouts. ◆
8. **Server work over SSH:** `pneu account` forwarding and the consent
   forward.

## As built: step 3

Where the code differs from the plan above:

- **Route fields.** `Policies` and `Types` are one map, class to allowed
  media types (`routes.go`). Entries also carry `Handler` (the server's),
  `Local` (`/open`, `/theme.css`, `/events`: the client answers them) and
  `Location` (none, local, or Gmail). There is no separate `part-image`
  class: an SVG part is `part-sandbox` with `image/svg+xml`, and the
  route's Dest rule allows that type only to an image destination.
- **Two more classes.** `data`: JSON, SSE, CSS, scripts and plain-text
  errors, the base set with no CSP; Check refuses HTML and SVG under it,
  on every route. `static-svg`: an SVG under `/static/` (the favicon), with
  `sandbox; default-src 'none'; style-src 'unsafe-inline'`, so navigating
  to it runs nothing; it still renders as an `<img>` and a favicon (checked
  in headless Chromium, whose favicon fetch is `Sec-Fetch-Dest: image`).
- **Errors on part and static-svg answers** (ServeContent's 416 and 412)
  switch to `data` `text/plain` without the file's disposition; Check
  allows `data` on the part route only at status ≥ 400. A multi-range
  request has its `Range` dropped and gets the whole file as a 200 (parts
  and static): no route answers `multipart/byteranges`.
- **1xx responses** a handler writes are swallowed by the policy writer;
  the class applies at the first status ≥ 200 (N5, server side).
- **Cache is the route's, on every response,** errors included:
  `no-store` (the old `private, no-store` lost `private`, which adds
  nothing to `no-store`). `/static/` sends `no-cache`. In server mode it
  also sends a strong ETag (its content hash; embedded files have no
  modtime), so the browser revalidates with a 304: the server is the
  source of truth for its own files. **N2 stands for the proxy:** the
  client strips `If-None-Match`, `If-Modified-Since`, `If-Match`,
  `If-Unmodified-Since` and `If-Range` from `/static/` requests and `ETag`
  and `Last-Modified` from the answers, and always fetches the body (the
  assets are small). Otherwise a compromised dell could serve a hostile
  `app.js` under the clean file's known ETag, and after the repair the
  honest server's 304 would keep the hostile bytes running. Check doesn't
  carry this: it validates what the server sends, and the stripping is
  the proxy's (step 5).
- **Unrouted responses** (middleware refusals, mux 404/405) are `data`
  `text/plain`; the mux's own trailing-slash redirect (`/static` →
  `/static/`) is allowed as a local redirect.
- **Send reservations (N7).**
  - The draft id is the form's `message_id`. A gmi failure is recorded
    `rejected`, freeing the id for another try with any content, only
    when it proves nothing went out: `ErrBusy` (gmi never started), or
    lieer exiting on its own before its "sending message" line, judged
    from a scan of the whole output stream (`gmi.Result.NotSent`), never
    from the 8 KiB tail. Everything after that line short of acceptance
    is `unknown`, whatever the HTTP status: lieer makes more requests
    after the send, and a later 4xx says nothing about the send.
  - **Known limit:** httplib2, lieer's transport, resends a POST whose
    connection dropped (`BadStatusLine`, `RemoteDisconnected`) even with
    `num_retries=0`. If Gmail took the first attempt, one `gmi send` can
    send twice. That is inside lieer's run and out of pneu's reach; pneu
    doesn't patch lieer.
  - Retention runs on one clock, the last submit: a replay durably
    refreshes the record's `submitted`, and compose.js's `idAt` is the
    last submit too. A submitted draft keeps its id whatever the clock
    says; only a discard drops it. Only a never-submitted draft's id
    expires client-side.
  - The draft id is in the localStorage draft, with a second copy in the
    sessionStorage marker. If localStorage doesn't keep it, the submit is
    blocked with a message.
  - Prune checks and removes each record under the same lock reserve
    writes under, and never touches an id in flight.
  - A replay whose refresh write fails (a full disk) is still answered
    from its record and pinned in memory: Prune keeps it and retries the
    write on every pass until it lands. **Residual:** if the server
    crashes before that, the pin is lost and the record's previous
    deadline applies.
  - "Scanned" means gmi exited on its own and pneu read its output pipe to
    a clean EOF (pneu owns the pipe; exec's Wait would report the exit
    over an unfinished drain). A descendant holding the pipe past the
    WaitDelay makes the run not Scanned, so never `rejected`.
  - Every gmi run pneu reads sets `PYTHONIOENCODING=utf-8` and
    `PYTHONUTF8=1` over whatever it inherited (`gmi.PythonEnv`): under
    an inherited utf-16 the ASCII markers wouldn't match, and an accepted
    send could read as failing before its send line.
  - Trimming the stored drafts (20 kept) never evicts a submitted draft
    or the one being sent, and runs before the persist and read-back, not
    after.
  - A 304 or 204 carries no `Content-Disposition` (the policy writer drops
    the part's; Check requires none).
  - Mailto unsubscribes go through the same log, keyed by account, message
    and exactly what the mailto sends: a second preview of the same one is
    the same send, across restarts; `unknown` answers `maybe-sent`.

## As built: step 4

Where the code differs from the plan above, or settles what it left open:

- **Registration waits for the whole handshake.** Go calls
  `VerifyConnection` *before* it checks the client's CertificateVerify, so
  at that point the client has shown the paired certificate (which is
  public) but not proved it holds the key. `VerifyConnection` checks the pin
  and runs whois; the connection is registered (under the lock a reload
  takes, the peer checked again) only once the handshake has finished. So
  the listener does its own handshakes, off the accept loop (at most 32 at
  once per address, 10s each), and hands net/http only connections that
  are done and registered. A test shows a paired certificate with the wrong
  key is never registered.
- **Which routes the peer listener serves** comes from the table:
  everything but `Local` routes, so `/theme.css` goes as well as `/open`.
  `/events` is `Local` (the client answers browsers itself) but also
  `Upstream`: the client daemon's one upstream stream reads it from the
  server. `/peer/hello` is `PeerOnly`. It answers `{protocol, name,
  revision, modified, epoch, gen}`; `name` is the hostname.
- **`peers-reload <generation> <hash>`**, not just the generation; the ack
  is `ok <generation> <hash>`. Both ends allow it 5s (`ReloadTimeout`), the
  daemon's own wait for closing connections 4s. A daemon without a peer
  listener answers `error peers off`, which the CLI reports as
  "next start". A removal is acknowledged once the peer's connections have
  left net/http (`ConnState` closed) **and** every handler on them has
  returned (an SSE stream, a download blocked on flow control); past 4s it
  answers an error, and the CLI says pending. A connection stays in the
  registry until it's finished in both senses, however it closed: a client
  that started a `/tag` or `/send` and hung up before the removal has a
  closed connection whose handler still runs (tag writes and sends outlive
  their request's cancellation), and the reload waits for it too. A reload
  that timed out leaves them registered, so the next one waits again.
  HTTP/2's serve loop doesn't wait for its handler goroutines, so a handler
  that starts after its connection is finished is refused before it runs.
- **No peer listener without the control socket.** Without it no removal
  could be acknowledged. The socket starts before the peer server loads
  `peers.json`, and a reload before `Start` just loads the file, so a
  `pneu peer add|remove` that races the daemon's start is either answered
  or finds no socket ("next start") and is loaded at start. A reload never
  goes back a generation.
- **CLI.** `pneu peer add --stdin` needs the `peer` block (its port goes in
  the answer) and tailscaled `Running` (its StableID does too). It refuses
  this machine's own node and key. Its stdout JSON carries one more field,
  `applied`: `live`, `next-start` or `pending`; it exits 0 in all three,
  since the file was written, and the client decides. `remove` exits 1 when
  pending. No `XDG_RUNTIME_DIR` (an SSH session without pam_systemd) means
  no socket to ask: pending, not "next start", since a daemon may be
  running.
- **Shapes.** Peer names are lowercase DNS labels of at most 32; nodes are
  alphanumeric, at most 64. The add request is read token by token:
  exactly the keys `name`, `node`, `origin`, `cert`, lowercase, once each,
  string values (encoding/json would keep the last of a duplicate and match
  `Name` to `name`). The certificate, trimmed, must be one PEM block with no
  headers, starting the input, with one `-----BEGIN ` in all: pem.Decode
  skips an unreadable block to a later one. `peers.json` is
  `{generation, hash, peers}`, the hash SHA-256 over the JSON of
  `{generation, peers}`; a file whose hash doesn't match, or that isn't
  exactly 0600 and ours, loads as an error: at start that means no peers.
  The server's key and certificate are one file, `peer/server.pem`, so one
  link creates both.
- **Leases.** Each connection has its own timer set to its expiry, so it
  closes at 60s without a renewal, not at the next sweep. The first lease
  runs from the handshake's whois, not from registration: a client holding
  the key could otherwise stall its CertificateVerify up to the 10s
  handshake deadline and stretch the bound to 70s. A renewal whois
  that *answers* and fails the predicate closes the connection at once; one
  that errs or times out leaves it to the timer. Reconcile closes the
  connections on an address that's gone (all of them when tailscaled isn't
  `Running`), not only the listener.
- **LocalAPI.** `GET /localapi/v0/status?peers=false` (`BackendState`,
  `Self.ID` (the StableID; `NodeID` is the numeric one), `Self.UserID`,
  `Self.TailscaleIPs`) and `GET /localapi/v0/whois?addr=<ip:port>`
  (`Node.StableID`, `Node.Addresses` as prefixes, `Node.Tags`,
  `Node.Sharer`, `UserProfile.ID`; 404 for no match). Checked against
  tailscale 1.102.4: tailscaled refuses any Host but
  `local-tailscaled.sock` (403); these GETs don't need `Sec-Tailscale:
  localapi`, which is sent anyway. `Tags` and `Sharer` are omitted when
  empty. Answers are capped at 1 MiB. The socket must be root's and a
  socket (`Lstat`) before every call.
- **Reauth over the link** (`POST /accounts/{a}/reauth`) already answers
  `409 reauth-on-server`, ahead of step 8.
- **Config.** `server` is parsed and kept as written (so `pneu account
  add` round-trips it) and refused: with `accounts`, as both; alone, as
  not built yet (step 5).

## As built: step 5a

The client daemon's link, pairing and proxy. Events fan-out, status.json
v2, the theme watcher, the error page and `/client/*` are step 5b (below).

- **Packages.** `internal/link` is the client's side of the link:
  credentials, the pre-dial checks, the pinned transport, the lease, hello
  and the state. `internal/client` is the daemon's HTTP side: Auth, local
  routes, the proxy and the response boundary. The boundary's header work
  is `web.Checker.Admit` and `web.Admitted.Install` (`internal/web/proxy.go`),
  shared code next to Check and the policy writer, so the class and cache
  rule are applied by the same writer the server's handlers use.
  `cmd/pneu/client.go` has `pneu client pair|unpair` and `serveClient`;
  `pneu serve` runs it when the config has `server`.
- **Credentials.** `$XDG_STATE_HOME/pneu/peer/client.pem` (key and
  certificate, one file, as the server's `server.pem`) and `pin.json`
  (`name, ssh, node, port, spki, cert, protocol, paired`; unknown fields
  refused; the SPKI must be the certificate's, and not our own key). The
  directory is `PrepareDir`'s 0700, both files exactly 0600 and ours,
  `O_NOFOLLOW`, checked on every load. The daemon refuses to start when
  the config's `server.node` isn't the pin's. `config.json`'s `server` is
  `{ssh, node, port}`, validated on load; `server` with `peer` is refused
  too, and `pneu account add` refuses on a client.
- **Pairing.** The request's `name` is `-name` or the hostname's first
  label, lowercased, and must pass `peer.ValidName`. The answer is read
  through a 16 KiB cap (the write past it fails, ending ssh's output) and
  parsed by `peer.ParseAddResult`, the token walker `ParseAddRequest` uses:
  exactly its six keys, once each, spelled exactly, `port` and `protocol`
  integers. A protocol mismatch pins nothing and prints the `peer remove`
  that undoes the server's record. `unpair` deletes the pin and the key (a
  re-pair makes a fresh key) and prints `ssh <target> pneu peer remove
  <name>`; it doesn't run it. The ssh binary is a seam; the tests run a
  stand-in that exits 99 unless argv is exactly pairing's.
- **LocalAPI.** `GET /localapi/v0/status?peers=true` (8 MiB cap) adds
  `Peer`, a map keyed by node key whose values' `ID` is the StableID, with
  `Online` (a bool, present when false), `TailscaleIPs` and `LastSeen`;
  `Tags` is absent when empty. Checked against tailscale 1.102.4 through
  the socket and `tailscale status --json`. The server's address is its
  first IPv4 one, else IPv6.
- **One more reason.** `node-mismatch`: whois at the server's address fails
  the predicate. Folding it into `refused` would tell the user pneu isn't
  answering when the tailnet says the address is someone else. `starting`
  is the state before the first attempt ends. A whois that doesn't answer
  is `tailscale-down`; a node missing from the map is `node-offline`.
- **Lease (N9).** A session is one up period: its own `http.Transport`
  whose `DialTLSContext` dials only the checked address, and the set of TCP
  connections it made. The lease runs from the pre-dial whois; every 10s
  the link renews it with under 20s left, and a timer closes it at 60s.
  Going down, for any reason, closes every connection in the set, not just
  idle ones: a busy HTTP/2 connection (a download, the future SSE stream)
  would otherwise keep running under the authorization it was checked
  with, and `CloseIdleConnections` doesn't touch it. A renewal whois that
  refuses closes at once (`node-mismatch`); one that doesn't answer leaves
  it to the timer (`tailscale-down`). A new dial inside a session doesn't
  rerun the pre-dial check: the session's lease covers it.
- **Transport.** `link.PinnedTLS` is the one place `InsecureSkipVerify`
  is set, with `VerifyConnection` requiring one certificate with the
  pinned SPKI (an empty pin refuses all). HTTP/2 only (`Protocols`, and the
  dial refuses a connection that didn't negotiate `h2`), `Proxy: nil`, no
  `ClientSessionCache`, one idle connection, identity encoding
  (`DisableCompression`, and `Accept-Encoding` never goes up), 64 KiB of
  response headers (past it the HTTP/2 client drops the connection:
  a server's own link to kill), and HTTP/2 pings after 30s idle.
- **Header timeouts are per route** (`web.Route.Wait`, default
  `DefaultWait` = 10s): `/send` and `POST /unsubscribe` 5 minutes, `/tag`
  and the unsubscribe preview 30s, a part zip a minute. A flat 10s would
  make every slow send's outcome unknown. The transport's own
  `ResponseHeaderTimeout` is the 5-minute backstop.
- **Hello** answers `accounts: [{name, email}]` too. That's where the
  client gets the address a `/gmail` redirect's `authuser` must be: the
  server is the only one that knows it, it's in the handshake before any
  page is proxied, and trusting it costs nothing, since a server that lies
  about its own accounts can only point a redirect at
  `mail.google.com` under the fixed shape. Hello is bounded (64 KiB, 16
  accounts, names by `config.ValidName`, emails printable with one `@`,
  revision kept only as 40 hex); a protocol mismatch is judged before the
  rest is parsed. `web.Protocol` stays 1: no client was ever released.
- **Routing.** `web.ClientRoutes` builds the mux from the table without
  `PeerOnly` routes. Local: `/open` (Auth.Open), `/theme.css` (this desk's
  theme, `web.ServeTheme`), `/events` (503 until 5b). Before the mux, a
  request with any `Service-Worker` header gets a 404, and `CONNECT` and
  upgrades are refused.
- **Requests up.** A placeholder authority that `Link.RoundTrip` replaces
  with the checked address (so the Host the peer listener checks and the
  address dialed can't disagree); the path and query as the browser sent
  them; minus hop-by-hop, anything `Connection` names, `Cookie`,
  `Authorization`, `Origin`, `Referer`, `Forwarded`, `X-Forwarded-*`,
  `X-Real-Ip`, `Upgrade`, `TE`, `Trailer`, `Accept-Encoding` and any
  `Pneu-*`; on `/static/` also the five validators (S8).
- **Answers down.** `ModifyResponse` runs `Admit`, which runs Check, then
  keeps: one `Content-Type` re-emitted with at most a token `charset`;
  one all-digit `Content-Length`; on part routes a well-formed
  `Content-Range` and `Accept-Ranges: bytes|none`; `Content-Disposition`
  rebuilt from its parsed type and filename (valid UTF-8, `cleanFilename`,
  255 bytes, `rfc5987`); `Location` as Check resolved it (local ones
  become absolute on the client's origin). Any `Content-Encoding` but
  identity is refused, as is a 304 on `/static/` (asked without
  validators, a 304 would keep what the browser has). The response's own
  header map is then emptied. The final writer swallows 1xx (and resets
  its scratch map), installs the admitted set on the Auth middleware's
  policy writer at the first final status, and after that hands out a
  map nothing reads, so trailers go nowhere.
- **Idle.** Each proxied body's `Read` arms a 60s timer that cancels that
  request's context (an HTTP/2 stream reset); the timer runs only while
  waiting on the server, so a slow browser doesn't count.
- **Not sent or unknown (R10, N6).** A local answer carries `Pneu-Link`:
  `not-sent` (502) when no connection was ever handed to the request
  (`httptrace.GotConn` never fired: the link was down, or the dial or TLS
  handshake failed), `unknown` (504, or 502 for a refused answer) otherwise.
  GotConn fires only after the pinned handshake, so it's the line between
  "nothing could have left" and "bytes may have". One conservative edge:
  an HTTP/2 request retried onto a new connection after its first one died
  unused has already seen GotConn, so it reads as unknown. Mutations get
  `{"ok": false, "error": …, "link": …}` (tagFail's shape), GETs
  text/plain. The page's rendering of it is 5b.
- **Launch.** `launch` retries the link now (a probe when up) and, in the
  background, once the link is up within 15s, `POST /sync?reason=launch`
  upstream. Launches while one waits collapse. A `POST /sync` from the page
  while the link is down retries it too (R and focus). The control socket
  in client mode answers `launch` and `status`; `peers-reload` answers
  `error client mode…`.

- **Review fixes (Codex C1–C3).**
  - *Untyped answers (C1).* net/http sniffs a type into any body that
    has none, nosniff or not, after the proxy has admitted it. So only
    301/302/303/307/308 are redirects, and only on routes whose `Location`
    rule allows one; any other 3xx but 304 fails Check. Only 204, 304 and
    those redirects may be untyped (`web.Untyped`). An untyped answer is
    `Admitted.Bodiless`: the proxy closes the upstream body, forwards
    none, and a redirect says `Content-Length: 0`. The final writer
    refuses a typed-less answer that isn't bodiless (502), and a body
    write without a type. The server's policy writer mirrors it: a
    non-untyped status with no type gets `text/plain; charset=utf-8` at the
    header write, and a body after an untyped header is refused
    (`ErrUntypedBody`).
  - *Lease from the whois (C2).* The lease runs from before the pre-dial
    whois; the close timer is armed for what's left of it after hello, a
    session whose lease ran out during hello is never published
    (`tailscale-down`), and `RoundTrip` refuses once it has lapsed.
  - *Unlink (C3).* The control socket's `unlink` (client daemon only, no
    arguments): the link drops its identity and pin, closes every
    connection of its session and of an attempt in flight, reports
    `not-paired`, and never reconnects, not even on demand; the ack comes
    after the connections are closed. `pneu client unpair` sends it
    first: no daemon, it goes on; no ack (or no `XDG_RUNTIME_DIR` to ask
    with), it removes nothing and says to stop the service and retry.

- **Review fixes, round 3 (D1–D2).**
  - *Lifecycle lock and required socket (D1).* The client daemon takes an
    exclusive, non-blocking flock on `peer/daemon.lock` (a file never
    replaced or removed), then its control socket, both before it loads
    any credentials; without either it exits ("another pneu client is
    running", "client mode needs its control socket"). An `unlink` that
    arrives before the link exists stops it from starting. `pneu client
    unpair` sends `unlink`; on an ack it deletes (the daemon has already
    dropped the pairing from memory). If nothing answers (or there's no
    `XDG_RUNTIME_DIR` to ask with) it takes the same lock: held means a
    daemon runs without a reachable socket, so it removes nothing and
    says to stop the service; free, it holds the lock across deleting the
    credentials and rewriting the config, so no daemon starts mid-delete.
  - *Unpair owns every session to the end (D2).* The link keeps every
    session in a set from creation until its close has returned: an
    attempt's before it's published, the live one, and ones going down
    (`setDown` stops it being live but not owned). Pending to live is one
    step under the lock, refused once unpaired. A session's close runs
    once and a second caller waits for it, so `Unpair` (and a concurrent
    second `Unpair`) returns only when the set is empty: every
    connection closed.

- **Review fixes, round 4 (E1–E2).**
  - *The unpair transaction (E1).* A second stable lock, `peer/pair.lock`,
    spans `pneu client unpair` from before it sends `unlink` until the
    credentials and config are gone (10s wait, then it refuses), and spans
    a starting daemon from taking `daemon.lock` until its credentials are
    loaded and its link started. A daemon restarted after the old one
    acked waits for the unpair and then finds nothing to load.
  - *Dials in flight (E2).* A session counts each dial from its closed
    check until the socket is registered or closed. Close cancels dials
    in progress (the session's own context), closes the registered
    connections, and waits for the count to reach zero, so a socket
    established while it closes is gone before Unpair returns.
- **Review fixes, round 5 (F1–F2).** Codex closed E1 and E2 and found two
  more, fixed directly with mutation-checked tests.
  - *Close waits for close (F1).* A tracked connection's socket close and
    its removal from the set are one `once.Do`, so a second Close (Unpair
    racing a failed handshake's close) waits for the first to finish. Go's
    own second Close returns before the descriptor is gone.
  - *Pair is a transaction too (F2).* `pneu client pair` holds `pair.lock`
    from reading the config to writing it, and `unpair` reads the config
    only after taking the lock, so neither acts on the other's half-written
    state.

## As built: step 5b

The client daemon's live side: the event pipeline, local theme,
status.json v2, the error page and `/client/*`, and Reset window data's
daemon half.

- **Server side.** Every status-file rewrite also broadcasts SSE `status`
  with the same doc, built once (`web.WriteStatusFile` writes either
  version). The last doc rides in `/events`' `hello` (`status`, null
  before the first write), taken under a third lock, `pubStatus.mu`,
  which each broadcast holds; `subscribe` takes `marks.mu`, `view.mu`,
  `pubStatus.mu` in that order. `syncing`, `sync`, `auth` and `theme`
  have typed shapes in `web` (`SyncingEvent`, `SyncEvent`, `AuthEvent`,
  `ThemeEvent`), and `AccountView`, `ViewEvent`, `ThreadRef`, `StatusDoc`
  and `HelloEvent` are exported, so the client decodes the server's own
  types. `web.Protocol` stays 1.
- **One upstream stream** (`internal/client/events.go`). `GET /events`
  over the link whenever it's up (`WaitUp`), with the 10s header wait, a
  `text/event-stream` answer and the right `Pneu-Protocol`. The reader
  holds at most 64 KiB of any event: a longer line or event is read
  through and marked, never buffered; comment blocks (the preamble, pings)
  make no event and don't count toward the next. The first event must be
  `hello`: its epoch hex, at most 16 accounts, each in **the link's
  account set** (the one `/peer/hello` named, which lists every account
  whether or not the server has a sync engine), anything else and the
  stream is closed and reopened with backoff (1s to 30s). After it only
  `syncing`, `sync`, `account`, `auth`, `status` and `view` pass, each
  decoded into its `web` type and rebuilt field by field: account names
  in the set, `gmi` states, phases and ops as enums, counts not below
  zero, a percent only in 0–100, times re-formatted from RFC 3339 (else
  null), window ids and thread ids by pattern, a view's threads at most
  1000; free text (an account's error, an auth error) through `plain`:
  valid UTF-8, no control, bidi-override or line-separator characters,
  512 runes. A `view` must carry hello's epoch and a gen above the last
  one taken. Anything else is dropped (logged at most once per 10s). A stream that ends with the link still up asks the
  link for a probe; one that ends with the link reopens as soon as it's
  back.
- **Silence.** Every byte from the server resets a 60s timer; when it
  fires, `Link.Stalled(id)` takes the session down (`refused`, "the event
  stream went quiet") and kicks an attempt, which finds the real reason.
  `id` is the session that carried the stream (`Link.RoundTripOn` names
  it): a stall reported after that session was already replaced leaves
  the new one alone (review G3).
- **Work budget** (review G2). Two token buckets per stream, charged
  before anything is parsed: every byte off the wire (comments, discarded
  and oversized input included, so a line that never ends runs out of
  budget) and every block a blank line ends (events and comments). The
  defaults (`client.DefaultBudget`): 256 KiB/s with a 2 MiB burst, 50/s
  with a 200 burst; a healthy server sends a few events a minute, a ping
  every 25s and one hello of at most 64 KiB per connect. Over either, the
  stream is closed and reopened with backoff (1s doubling to 30s), which
  resets only once a stream has lasted 60s healthy, never on a valid
  hello alone. Dropped events and stream ends are logged at most once per
  10s, with a count of what was suppressed. A `Hub` with no subscribers
  marshals nothing; the daemon's state still updates.
- **The wire** (review G4). Lines end in LF, CR or CRLF, a CRLF split
  across reads being one end; one leading UTF-8 BOM is skipped; `id` and
  `retry` are ignored; a block dispatches only if it carried a data field
  (or ran past the cap), so an event name alone, or `id`/`retry` alone,
  before hello doesn't fail the handshake.
- **Account names** (review G1). `config.ValidName` is now the plain-name
  rule everywhere: valid UTF-8, graphic characters only (no control,
  format or bidi characters, no space or line/paragraph separator), no
  `/`, not starting `-` or `.`, at most 64 bytes. A server's config can't
  hold another name, the link refuses a `/peer/hello` naming one
  (`protocol`), and the handshake checks the set again. Rejected, never
  cleaned.
- **Local fan-out** (`live.go`). The daemon's own `web.Hub`. One mutex
  spans every change to the daemon's state and its broadcast: the
  handshake (state replaced, and a fresh `hello` to every open page, so a
  page with an unknown outcome reconciles by gen), each relay (`account`
  replaces that view; `syncing`/`sync` set its `running` as app.js does;
  `view` moves gen; `status` replaces the doc and its arrival time), each
  `link` and each `theme`. A page's `/events` subscribes and encodes its
  `hello` under the same mutex: `{epoch, gen, accounts, status, link}`,
  with `link` `{state: starting|up|down, since, reason, server, revision:
  {client, server}}`. The state survives the link going down: a page
  opened then renders the last-known views. `LinkChanged` (the link's
  `OnChange`) reads the link's state under that mutex rather than taking
  the call's: `OnChange` runs outside the link's lock, so two changes'
  calls can arrive in either order.
- **The server's name** in pages, the error page and status.json is the
  SSH target the user paired with (`config.server.ssh`): local text,
  known with the link never up. Hello's `name` is never shown.
- **Theme.** `web.WatchTheme(ctx, path, every, changed)` is the watcher
  both modes run on their own desk's file; the daemon broadcasts its own
  `theme`. An upstream `theme` is never accepted.
- **status.json v2** (`statusfile.go`): `{version: 2, updated, running,
  unread, senders, accounts, server: {name, link, linkSince, statusAt,
  reason, update}}`. `updated`/`running` are this daemon's; the counts are
  the last valid upstream status (from hello or an event), held to ≤16
  accounts, ≤5 senders, account names in the set, every string through
  `plain` at 128 runes (a status past a limit is dropped whole); `link` and
  `reason` are the link's local codes, `reason` null while up; `statusAt`
  is when that status arrived here, null before one did; `update` is null
  until step 7. Writes: at start, on every wake (a status, a link change)
  but at most one per second, reading the state at write time (so the
  latest wins and nothing else is held pending), every 5 minutes, and
  `running: false` when the daemon stops. Markup in a sender name is kept
  as text (QML renders PlainText; JSON isn't HTML-escaped). **The bar
  widget accepts only version 1 until step 6**, so on a client it shows
  `status.json unreadable` meanwhile; the server keeps writing version 1.
- **Error page** (`errorpage.go`, `page/`). A GET navigation
  (`Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document`,
  `web.Navigation`) on a route that may answer HTML gets it whenever the
  link isn't up, and also when the send fails not-sent: 503, `Pneu-Link:
  not-sent`, under the route's HTML class (`app`, or `compose` on the
  form's routes; `web.HTMLPolicy`) installed through `Admitted.Install`.
  A script's fetch, a frame, or any other route gets the plain not-sent
  answer as before. The page is this binary's template and
  `/client/static/error.{css,js}`, with this desk's `/theme.css`; it
  names the reason in local words, the bar-menu action (Fix with agent,
  Update pneu / Update <server>) and the terminal command where one fixes
  it (`sudo tailscale up`; `pneu client unpair` then `pneu client pair
  <ssh>`; `ssh <ssh> systemctl --user status pneu`). `pin-mismatch`
  doesn't claim to retry, and nothing on any page offers to trust a key.
  Its script reloads when the local `/events` says the link is up (a
  `link` at once; a hello already up after a second, so a page served on
  a send that failed before the link noticed can't loop), and its button
  posts `/client/retry`.
- **`/client/*`** is three routes in the shared table, `ClientOnly`
  (Local, no server handler; neither of a server's listeners has them):
  `GET /client/link` (the `link` view as JSON), `POST /client/retry`
  (Auth's Origin and cookie, no query, no body, else 400; a probe when up,
  an attempt when down), and `GET /client/static/` (the two files by name;
  anything else, the directory included, a 404). Each answer goes through
  the policy writer under its route's class, and the client tests run
  Check on every answer the daemon makes itself (`web.RouteOf`).
- **Not-sent errors on `/send`** are text/plain: its route answers no
  JSON (`writeLinkError` follows the route's data types). 5a wrote JSON
  there, outside the contract.
- **Probes.** `launch` already retried (a 3s hello probe when up). `POST
  /sync` from the page (R, focus) now asks for one before it's sent,
  link up or down.
- **Reset window data, daemon half.** The control socket's `reset-window`
  (no arguments) arms `Auth.ArmClearSite`, in **both modes**: the next
  authenticated top-level navigation gets `Clear-Site-Data: "cache",
  "storage"`, once, set by the policy writer (`Clear-Site-Data` is one of
  the headers a class owns, so no handler or upstream sets it).
  Authenticated means a session cookie that validated, or `/open` with a
  good nonce (or its fast path, with the cookie); a navigation is
  `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document`, and
  `Sec-Fetch-Site` `none` or `same-origin`, on our Host. Anything else (no
  session, a failed nonce, another site's or a same-site page's
  navigation, a fetch or frame) leaves it armed (review G5). A worker's
  script fetch (`Service-Worker: script`) is still a 404. The menu flow
  is step 6.
- **app.js** (`web/static/link.js`, the pure half, `web/link.test.js`).
  A hello with `link`, and `link` events, set the line's link state; `down`
  is `Can't reach <server> · retrying` in `--accent-hot`, ahead of
  everything else. The details panel adds the link and both builds. A
  failed write whose answer carries `Pneu-Link` (header, or the JSON's
  `link`) toasts `Not sent: can't reach <server>.` or `<server> didn't
  answer; checking when it's back.` in place of its usual failure; the
  next hello's generation reconciles the list. With no `link` in hello (a
  server) none of it shows.
- **Review fix H1.** Codex closed G1, G3, G4 and G5; G2's budget held but a
  `sync` event's `at` of `0000-01-01T00:00:00+01:00` parses, becomes year
  −1 in UTC, and fails `MarshalJSON` in the Hub, which logged per event
  outside the drop path's limiter. Every relayed time must now stay a
  four-digit year in UTC (`sane`), or the event is dropped through the
  limited path. The allowlist test fails on any encode-failure log line.

## Review status

Round 2 (Codex, 2026-09-30) found R1, R2, R7, R13, R14, R16 and R17 closed
in the design. The rest were partly closed, and N1–N14 were revised in
here. It confirmed three things as sound: the control socket is out of
browser reach (browser JavaScript has no unix-socket capability, and no
HTTP route bridges to it); `VerifyConnection` plus
`SessionTicketsDisabled` in Go 1.27 covers resumption; and the plan's
claims about current code (the `/open` fast path, `data-accounts`,
label-only pushes, the widget's commands and `Text.PlainText`) are
accurate. A third round is due at build step 4 ◆, against code.

## Open questions

- Is a 60s whois lease the right length? It bounds how long a node that
  was just tagged or removed keeps access. Shorter costs a LocalAPI round
  trip more often, which is cheap. Watching the IPN bus for netmap changes
  would be tighter and much more code.
- Reset window data needs one click from the user in Chromium's site
  settings (step 2 of R4/N3). Is that acceptable for a recovery that
  should never be needed, or is it worth driving the browser's DevTools
  protocol to do it without the click?
