# Message actions: unsubscribe, primary link, link hints

Three keys on the thread pane:

- `X` — unsubscribe, after a confirmation that shows exactly what will happen.
- `o` — open the message's primary link (its CTA), shown in a chip first.
- `L` — label every link in the message; a label selects, `Enter` opens.

Every one of these acts on content the sender wrote. The sender is the
adversary. Scoring and JSON-LD are the sender's *suggestions*, never
evidence of legitimacy: a sender can manufacture the winning score. The
guarantee is only that nothing happens that the user didn't see, in full,
with the real destination, and confirm.

Reviewed adversarially (Codex, 2026-09-29) before any code; the numbered
notes (R1…) point at that review's findings.

## Which message

Each action binds to one message identity (account, message-id) at the
moment the key is pressed, and keeps it through preview, confirmation and
hints (R6):

- A key from inside a frame acts on the message that owns that frame. Each
  frame's key listener is installed with its identity; it does not consult
  the cursor.
- A key from the thread pane acts on the cursor message, which must be
  expanded (its chip visible). No fallback to another message.
- A pending dialog or hint session is cancelled when the cursor moves, the
  frame is replaced, or the thread re-renders.

## Keys and input

New activation keys (`X`, `o`, `L`, and every key inside a dialog or hint
session) ignore `e.repeat` (R7). A session consumes all keys from the keydown that starts it, including
while its preview is loading, before the normal key map, so nothing falls through to archive or reply.
Dialogs are `showModal()` (inert background, above any overlay). A
confirming control arms only after the dialog has rendered *and* the
initiating key has been released; any key event before arming is dropped.
Buttons are gated the same way, so a queued Enter/Space can't activate them.
Once armed, native inspection keys (Tab to the disclosure, Enter/Space on
it, arrows and PageUp/PageDown to scroll long fields) work inside the
dialog; they never reach the app's key map.

## Unsubscribe

### Source

Only `List-Unsubscribe` (RFC 2369) and `List-Unsubscribe-Post` (RFC 8058).
Never a body link.

`notmuch show --format=json` doesn't return these headers, so the server
reads the message file itself. The path comes from a headers-only lookup
(`Account.Headers`, `--body=false`; R12). The file is opened through
`os.OpenRoot(<account maildir>)` with the path made relative to it, so
nothing outside the account's maildir can be reached, symlinks included;
it must `Stat` as a regular file on the opened descriptor (R14). Headers
are read through a 256 KiB cap with `net/mail.ReadMessage`; DKIM
verification (below) streams the body into its hash, capped at 50 MiB
(past that, no one-click).

If notmuch reports several filenames, each is parsed, and the action is
offered only if they all yield the same action (R3).

### Parse

- Exactly one `List-Unsubscribe` and at most one `List-Unsubscribe-Post`;
  otherwise nothing is offered.
- RFC 2369 grammar, conservatively: after unfolding, a comma-separated
  list of `<URI>` items, each optionally followed by a parenthesized
  comment; whitespace inside the brackets is removed, as the RFC says it is
  ignorable. The first malformed item ends the list (items after it are
  never considered, as a compliant client wouldn't reach them). At most 8
  items, 2 KiB each. If the value is made of RFC 2047 encoded-words
  (some ESPs send it that way, and sign it that way), it is decoded with
  `mime.WordDecoder` first, charsets `us-ascii` and `utf-8` only, and the
  result must be ASCII; anything else offers nothing. The signature covers
  the encoded form, and decoding is deterministic.
- Each URI through `net/url.Parse`; supported: `https`, `http`, `mailto`.
  Rejected: userinfo, a non-ASCII or empty host, an IP-literal host,
  control characters.

### Choosing the method

- **One-click** is offered only when *all* hold (RFC 8058 §3.1, §4):
  `List-Unsubscribe-Post` is exactly `List-Unsubscribe=One-Click`; the list
  has exactly one `http(s)` item and it is `https`; and a DKIM signature
  verified by pneu covers both headers (below).
- Otherwise the first supported item **in the sender's order** (RFC 2369
  left-to-right preference): `mailto` → a mailto unsubscribe; `http(s)` →
  open in tab.

### DKIM (R: RFC 8058 §4)

pneu verifies, stdlib only: `DKIM-Signature` headers, `rsa-sha256` (≥1024
bit) and `ed25519-sha256`, `relaxed`/`simple` canonicalization, key from
`net.LookupTXT("<s>._domainkey.<d>")`. A signature counts only if it
verifies, has no `l=` (a body-length limit lets appended content ride on
it), and its `h=` lists both `List-Unsubscribe` and `List-Unsubscribe-Post`
at least as many times as each occurs (so every copy is signed; with
exactly one of each required above, a copy added after signing is refused
before DKIM is consulted). Not over-signing: in a sample of 491 real
messages (2026-09-29), 466 sign each header once and only 15 over-sign.
`x=` is not enforced: ESPs set it days out, so it took one-click from 135
of those 491, and what it guards against, replay, gets an attacker only
the genuine sender's genuine endpoint for whoever the message was first
sent to. The signature's own field never takes part in `h=` selection.
Header selection is linear (an index by name), `h=` is capped at 64
entries, and verification honors the request's cancellation. Gmail's
`Authentication-Results` is not consulted: nothing in the message proves it
is Gmail's, and it doesn't say what `h=` covered. The signing domain (`d=`)
is shown in the confirmation. Old mail whose selector has rotated simply
loses one-click and falls back.

### Preview and execution are one bound action (R3, R11)

`GET /unsubscribe/{account}/{msgid}` parses and returns the chosen action
plus a token. The server keeps `token → {account, msgid, action}` in memory:
128-bit random, expires after 2 minutes, single use. `POST /unsubscribe`
takes only the token and executes *the stored action* — it never re-reads
or re-selects. Consuming the token is atomic; the result is kept for 10
minutes under the token so a lost response can be asked about
(`GET /unsubscribe-result/{token}`) instead of retried. A mailto action is
assigned its outgoing Message-ID at preview time and goes through the same
in-flight claim compose uses, so it is sent at most once. The UI treats a
lost response as "outcome unknown" and checks the result, never offering a
fallback as though nothing happened.

`readOnly` accounts get no token for mailto (and no send).

### One-click request (R2, R12, R13)

The server POSTs `List-Unsubscribe=One-Click`
(`application/x-www-form-urlencoded`) to the https URL:

- Port 443 only (none written, or `:443`). An otherwise qualifying one-click
  URL on another port is a security refusal, not a downgrade to open-in-tab.
- IPv4 only. The server resolves A records itself; *every* answer must be
  a global unicast address outside IANA's IPv4 special-purpose registry
  (0/8, 10/8, 100.64/10, 127/8, 169.254/16, 172.16/12, 192.0.0/24,
  192.0.2/24, 192.88.99/24, 192.168/16, 198.18/15, 198.51.100/24,
  203.0.113/24, 224/4, 240/4, 255.255.255.255), else refused. It dials the
  first vetted IP through a `DialContext` that ignores the name, so DNS
  can't rebind; TLS verifies against the original name. IPv6 is not used:
  a network-specific NAT64 prefix can make a public-looking IPv6 address
  reach a private IPv4 one, and no prefix list can know that.
- No redirects (a 3xx is a failure), no proxy from the environment, no
  cookie jar, no Authorization or Referer, a fixed `User-Agent`.
- One 10s deadline covering DNS, connect, TLS, headers and body; response
  headers capped at 16 KiB, body read at most 64 KiB and discarded. At
  most 2 unsubscribes in flight server-wide.
- 2xx is success; anything else is failure.

A destination refused by the address check is a **security** refusal: the
dialog says so and offers nothing else. A transport failure (timeout, 4xx,
5xx, 3xx) may offer the next supported item in the sender's order, as a new
preview (R4).

### mailto (R1)

Parsed per RFC 6068, not as a form: `+` is literal, each `%xx` decoded
exactly once, then validated. The path must parse (`net/mail.ParseAddress`)
to exactly one mailbox, and the `to` hfield must be absent. Hfield names are
case-insensitive; a duplicated hfield rejects the mailto. `subject` and
`body` are honored (subject "unsubscribe" only when the hfield is absent;
a present value, even empty, is kept); each capped at 1 KiB after decoding,
the subject stripped of CR/LF/NUL, the body's line ends normalized. The
address is at most 254 octets (local part 64) and a plain dot-atom; quoted
local parts are refused, deliberately. A subject containing `=?` is sent
as one B encoded-word, so a recipient's MIME decoder reproduces exactly
the text the confirmation showed. Any other hfield (`cc`, `bcc`, `in-reply-to`, anything)
rejects the mailto rather than being silently dropped. The message is built
by a dedicated function that takes one address, not by compose's
recipient-list builder, and is sent from the account that received the
original.

### Confirmation (R9)

Built with textContent only. Every untrusted field is its own `<bdi>` with
`unicode-bidi: isolate`, has bidi/format controls (Cf) shown visibly as
`\u{XXXX}`, and is capped; the destination line has reserved space so a
long label can't push it out.

- One-click: "One-click unsubscribe", the destination as scheme + host +
  port if not 443 (ASCII; punycode stays punycode), "signed by `<d=>`", and
  the full URL behind a disclosure.
- mailto: the sending account, the recipient address, the subject, and the
  **complete body** as it will be sent.
- Open in tab: the full URL, and "opens in your browser".
- Context: `List-Id` if present, else the From address.

Confirm with `y` or the button, cancel with `Esc`/`n`, under the arming
rules above. After success the dialog offers `e` to archive the thread.

### Logging (R13)

Account, method, and a result category (`ok`, `refused-address`, `refused-port`,
`http-<status class>`, `timeout`, `tls`, `dns`). Never the URL, host,
address, or a raw Go error (a `*url.Error` carries the full URL).

## Browser-opened destinations (R4)

`o`, hints, and the unsubscribe open-in-tab fallback all pass one
client-side check at the moment of opening: http(s) only, not the app's
origin, not an IP-literal host, not `localhost`/`*.localhost`/single-label
hosts. They open with `window.open(url, '_blank', 'noopener,noreferrer')`,
like a body link click. A browser navigation is a different operation from
the server's POST, with the browser's credentials and redirects; the UI
never presents it as a fallback for a security refusal.

## Primary link (`o`)

1. **Declared action.** schema.org JSON-LD. The script is read from the
   same DOMParser document DOMPurify sanitizes, in an
   `uponSanitizeElement` hook that captures the `textContent` of
   `script[type="application/ld+json"]` before DOMPurify removes it (R10).
   That is the browser's own HTML parse (comments, raw text and malformed
   tags handled per spec), of the body part pneu is rendering and nothing
   else, and nothing captured is ever reinserted as markup. Bounds: 4
   blocks, 64 KiB each, 2000 JSON nodes, depth 6 (R12). `potentialAction`
   of `@type` `ViewAction`, `TrackAction`, `ConfirmAction`, `SaveAction`,
   `RsvpAction`; `url`, or `target` as a string or `{url}`. `urlTemplate`
   is refused, `@context` is never fetched; exactly one qualifying action,
   else none; the URL must be absolute.
2. **Button heuristic.** Runs *after* the frame has laid out, on its live
   (script-less) document (R5). Candidates: anchors with an http(s) href,
   at most 500 considered. A candidate must be visible: non-zero client
   rect inside the document, no ancestor with `opacity` < 0.5,
   `visibility:hidden`, `display:none` or clipping it away, and the element
   at its rect's centre (`elementFromPoint`) is the anchor or inside it.
   Score: button-like styling (computed background differing from its
   container's, padding, border-radius), 1–5 words of visible text with a
   leading verb (view, confirm, verify, track, pay, reset, sign in, accept,
   join, review, download, open, get started, activate), early in the
   document; minus footer text (unsubscribe, preferences, privacy, terms,
   view in browser, manage). One link only if it clears a threshold and
   beats the runner-up by a margin.
3. Plain text: not in the first cut.

### Showing it

The chip in the message header: `o  <label> → <destination>`, with the
label and destination rules of the confirmation (bdi, controls visible,
capped, destination space reserved, port shown). The destination is always
derived from the URL that will open, never from text. Known click-tracking
redirectors get `(redirect)`; any host may redirect, and the chip doesn't
imply otherwise.

The highlight of a heuristic pick is drawn in the app document, as an
overlay clipped to the frame, not by styling inside the frame, where the
sender's CSS comes after ours and could hide or fake it (R5). Before
opening, the pick is re-validated: the anchor is still connected, still
visible, and its href unchanged.

## Link hints (`L`) (R8)

Labels are drawn in the app document over the frame, from each anchor's
rect intersected with the frame, the thread pane and the viewport;
anchors with nothing left are skipped. The overlay is clipped to the pane
and sits below dialogs. At most 200 labels, home-row alphabet. The
label → URL map is frozen when hints open; a resize, scroll, frame
replacement or cursor move closes them. Typing a label *selects*: the
status line shows the full destination (confirmation rules), and `Enter`
opens it through the browser-opened check (mailto opens compose, as a
body click would). `Esc` closes.

## Why these lines

- Bound preview tokens: what the user confirmed is what runs, even if the
  file, the database or the header parse changes in between.
- DKIM we verify ourselves: RFC 8058 asks receivers not to offer one-click
  without it, and a header Gmail supposedly wrote is still just a header.
  It doesn't make a malicious sender safe; it makes the header theirs.
- IPv4-only pinned dial: the server sits behind the user's NAT, next to
  router admin pages and dev servers; the POST is the one request whose
  destination a stranger picks.
- JSON-LD from DOMPurify's own parse: correct HTML parsing without a second
  parser, and without a hand-written Go tokenizer.
