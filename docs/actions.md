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

   Decided in building it (2026-09-30):
   - *Which scripts.* An HTML-namespace `script` whose `type`, trimmed of
     ASCII whitespace and lowercased, is exactly `application/ld+json` (a
     parameter, `;x`, disqualifies), whose every ancestor is an HTML
     element up to `<html>`, none of them `noscript` or `template`. DOMPurify
     drops both of those whole without visiting their content, so the
     ancestor rule is a second wall; it is what refuses a script under
     `<svg>` or `<math>`. The hook is added immediately before the one
     `sanitize` call and removed in a `finally`: DOMPurify's hooks are
     global, and the viewer's calls must not see it.
   - *Bounds.* Past any of them there is no action at all, not the action
     found so far: what went unread might have been a second one. 64 KiB is
     UTF-8 bytes. Depth counts the block's top value as 1 (GitHub's shape
     is 4 or 5 deep). A block that isn't JSON contributes nothing, and the
     others still count (a `</script>` inside a JSON string ends the script
     there, leaving two broken blocks around real markup; hostile 46).
   - *Where.* `potentialAction` is read on any object, `@graph` and nested
     ones included. `@type` may be a list; a bare name (`ViewAction`), the
     full IRI (`https://schema.org/ViewAction` or `http://…`) and the
     compact `schema:ViewAction` all count, since senders write all three
     and they name the same type. Exactly those prefixes, case-sensitive:
     `@context` is never read to expand anything else.
   - *One.* Identical duplicates (same URL after normalization, same full
     name, compared before the 60-point cap) count once; two names that
     differ only past the cap are two actions, so none. Both `url` and `target` given, they must agree. A
     qualifying action with an unusable URL still counts toward "one", so
     it can't be dropped to promote another.
   - *URL.* Must start `http://` or `https://` literally and contain no
     whitespace (Unicode's, `\s`, plus U+0085 and U+180E), C0 or C1 control,
     format character (Cf), lone surrogate or backslash (the URL parser accepts
     `https:host`, backslashes and embedded newlines), be at most 4096 code
     points, and pass the browser-open check, which also refuses a user name
     in the address (`https://github.com@evil.example/`) for everything that
     uses it.
   - *Name.* Runs of ASCII whitespace become one space; empty or missing, it
     is the type (`View`, `Track`, `Confirm`, `Save`, `RSVP`); capped at 60
     code points with `…`; drawn with controls visible.
   - *Redirectors.* Exactly `list-manage.com`, `sendgrid.net`,
     `mandrillapp.com`, `mailchi.mp` and their subdomains. A `click.*` or
     `links.*` host is not evidence.
   - *No dialog.* The chip is the preview. `o` needs the message expanded
     and its chip rendered, ignores repeats, and runs the browser-open check
     again at open time; a click on the chip does the same.
   - *In view.* Being the preview, the chip must be on screen when `o`
     opens it: its whole rect inside the thread pane ∩ the viewport less
     every piece of app chrome that is sticky or fixed and overlaps the
     pane, in either layout (the page header, the split's sticky titles,
     the key bar; `usable`), and nothing else in front of it: hit-tested at
     its centre, its top and bottom rows, and each line of its name and
     destination (a wrapped destination is several). Otherwise — scrolled away while reading a tall body, say —
     `o` scrolls the chip into view and marks it for a moment, and opens
     nothing; a later, separate `o` (repeats never count) that finds it in
     view opens. The same from inside a frame. A click needs no such check:
     the chip was on screen to be clicked. (`primaryKey`, `primary` in
     actions.js.)
2. **Button heuristic.** Built 2026-09-30 (`guess` in actions.js). With no
   declared action, it runs *after* the frame has laid out, on its live
   (script-less) document (R5). Candidates: `a[href]` with an absolute
   http(s) href that passes the browser-open check (never a mailto), at
   most 500 considered. A candidate must be visible: non-zero client rect
   inside the document, no ancestor with `opacity` < 0.5,
   `visibility:hidden`, `display:none` or clipping it away, and the
   element at its rect's centre (`elementFromPoint`) is the anchor or
   inside it; its text at least 9px and not near-invisible against its
   background. Score: button-like styling, 1–5 words of visible text with
   a leading verb, early in the document, large; minus footer text,
   links with no drawn text (never picked), the last quarter of the
   document, and text that is an address. One link only if it clears a
   threshold and beats the runner-up by a margin.

   *Not a security boundary* (after Codex's review of it, 2026-09-30). A
   sender can already name any destination for `o`, invisibly, with
   JSON-LD (tier 1). The boundary is that the chip always shows the real
   destination, from the href that opens, and `o` opens only what the chip
   shows. The visibility checks below exist so that a guess is plausible
   (a link the reader can see, saying what it says); they refuse
   conservatively rather than model CSS paint exactly, and a real button
   they refuse just means no guess.

   Decided in building it (2026-09-30):
   - *Visible, precisely* (`seen`). The anchor's largest client rect, at
     least half of it inside the frame's viewport (which is the whole
     document: the frame is sized to its content) and inside every
     ancestor whose overflow isn't `visible`; the product of `opacity` up
     to the root at least 0.5; no ancestor (or the anchor) with a
     `clip-path`, `clip`, `filter`, `mask-image` or a `mix-blend-mode`
     other than `normal`, since each can make a box vanish while every
     rect says it is there (conservative: a real button with a drop-shadow
     filter is not guessed); the anchor itself `visibility: visible`; the
     visible part at least 8×8px; the hit test at its centre in the
     anchor. `display:none` needs no check of its own: it leaves no rects.
   - *Paint tricks, refused* (Codex, 2026-09-30). Each of these can put
     something over, in or behind a link that its rects and hit tests
     don't see, so any one refuses the link:
     - a `background-image` (an `url()` or a gradient) on the anchor, on
       anything inside it, or on any ancestor up to the root. What an image
       paints can't be known (it may not load; the eval has images off),
       so the old approximation of a gradient by its stops is gone;
     - a CSS animation or transition on the anchor, inside it or on an
       ancestor (`getAnimations()`): what shows now may not in a moment;
     - an overlay: any element that isn't the anchor, inside it or around
       it, that has `pointer-events: none` with a position other than
       static (the hit test passes through it), or is `fixed`, `absolute`
       or `sticky` with a background or any opacity, or is inside such an
       element (it paints in the same layer), whose box meets the
       anchor's. Found by one scan of the document per guess, counted in
       the budget, not by modelling paint order. A positioned `::before`
       or `::after` that paints has no rect to read, so it counts as
       covering the whole document: every link but its own element's is
       refused (hostile 58);
     - inside the anchor: an `<svg>` or `<math>` (anything not in the HTML
       namespace); a `::before`/`::after` whose `content` isn't `none`,
       `normal` or `""`, or an empty one that paints a background; a
       `::first-line` or `::first-letter` whose colour, fill, font size or
       background differs from its element's (checked on every element
       inside, whether or not the rule applies to it: conservative). The
       pseudo-element lookups run only when the frame's style sheets name
       one (read off the CSSOM, so escapes are undone), or on a `<q>`;
     - an opaque box inside the anchor (a background at least half opaque,
       or an image or other replaced element) that doesn't hold a piece of
       its text and covers a quarter or more of the text's area.
     `text-transform` is not refused: the label is the DOM text, and
     shown as that it says the same words the reader sees, cased
     differently at most.
   - *Budget* (`CTA.budget`). One guess visits at most 20,000 nodes
     (ancestors, the anchors' subtrees, the overlay scan, the style sheets'
     rules), measures at most 2,000 text ranges and looks up at most 20,000
     computed styles (each element's once per guess, cached). Past any of
     them the message gets no pick at all (`over`), not the best so far
     (hostile 61: one link with 2,100 text nodes before a clear button).
     Anything else that throws during a guess is likewise no pick
     (`error`), never a half-built chip or highlight, and never reaches
     the page: `guess` and `stillPicked` don't throw (the harness forces a
     throw on the thread page, `page-narrow-guess-throw`, and checks the
     body still renders with no chip, highlight, timer or page error).
   - *Visible text.* Every text node inside the anchor that draws (a
     box with area, `visibility: visible`) must be at least 9px, under 0.5
     opacity nowhere (the product from the anchor's), in no clipped,
     masked or filtered element, legible, and drawn where the link shows:
     half its box inside the anchor's visible box, and the hit test at the
     centre of that in the anchor. One piece that fails refuses the whole
     link, rather than dropping the piece: text hidden inside a visible
     button is a trick, not a label. The label is the drawn text (pieces
     on one line that touch run together, others get a space). A link with
     none (an image alone, an empty button) stays a candidate, so it can
     still be a runner-up, but is never picked: an image's `alt` is not
     what the reader sees, so it is never a label (hostile 59). Link hints
     label such links as any other.
   - *Legible.* The WCAG contrast ratio of the text colour
     (`-webkit-text-fill-color`, which is the colour unless set)
     composited over its backdrop, against that backdrop, at least 1.25.
     The backdrop is composited up the ancestors to the first opaque
     background (the frame's `html` always has one); a background image
     anywhere on the way refuses the link before this (paint tricks, above).
     Colours not in `rgb()` form (`oklab()`, `color()`) are
     converted by a 1×1 canvas. No separate alpha floor: faint text fails
     on contrast.
   - *Button-like* (`styling`). `styled`: the anchor's own background (a
     colour at least half opaque) differs from its
     container's backdrop, or an ancestor up to four levels whose only
     content is this link (same text, one `a[href]`) has one: email's
     table-cell buttons put the colour on the `td`. `padding`, `radius`,
     `border` on the anchor or that chain. The area scored is that cell's
     when there is one.
   - *Score.* A weighted sum, every weight, the threshold (6), the margin
     (2) and the visibility limits in one table, `CTA`, so the eval can
     tune them: styled 3, padding 1, radius 1, border 0.5, verb 3, short
     (1–5 words) 1, long (more than 8) −2, early 2 × (1 − centre / height),
     area 2 × (area / the largest candidate's), footer −8 (strong enough to
     sink a styled, early "View in browser"), no drawn text −3 (and never picked),
     tail (centre in the last quarter) −3, text that is an address −3.
     Verbs: view, confirm, verify, track, pay, reset, sign in, log in,
     login, accept, join, review, download, open, get started, activate,
     complete, continue, shop, read, reply, see, start, claim, schedule,
     book, RSVP, manage (your) order/booking/reservation/trip — at the
     start, after any leading symbols. Footer words anywhere: unsubscribe,
     opt out, preferences, privacy, terms, legal, view in browser/online/as
     a web page, manage (but not an order, booking, reservation or trip),
     help, contact, support, FAQ, social networks by name (and a bare
     "X"), app store badges, forward to a friend, update your preferences,
     careers, about us.
   - *One destination, one candidate.* Links to the same URL (after
     normalization) count once, the best-scoring one standing for it: a
     button and its text link, or two buttons to one page, are not each
     other's runner-up. So do links with the same label (whitespace
     collapsed, case folded) whose URLs share a hostname: eBay's two
     "Track package" links with different tracking parameters. Joined
     transitively (union-find over both keys), so one link never stands
     for two choices: `(A, "X"), (B, "X"), (B, "Y")` is one choice, listed
     once (Codex found `B` listed twice). One label on two hosts is two
     choices, which meet on the margin and the tie-break: a hidden or
     lesser `evil.example` "Track package" can't absorb, or be absorbed
     by, the real one (hostile 60). The margin is against the best *other*
     choice (hostile 55).
   - *Showing it.* The same chip, dashed, with a `guess` tag before the
     label; the label is the anchor's drawn text capped at 60; the
     destination, as always, from the href that opens, so a button whose
     text names one domain and whose href goes to another shows the
     other (hostile 57). The highlight (below) is on the cursor message
     only.
   - *When.* On the frame's `load` (after mailframe.js has sized it),
     every time a document is painted into it (a theme change, remote
     images on); a message collapsed before its frame laid out is guessed
     when it expands. A declared action always wins; the heuristic isn't
     consulted.
   - *At `o`* (`stillPicked`), after the chip's own gating: the frame holds
     the document the pick came from, the anchor is still in it, its
     `href` attribute is byte-identical, it is still visible by every
     check above, and its URL and label are the same. Otherwise nothing
     opens, the status line says why (`Not opened: the link changed after
     it was picked`), and the message is guessed afresh, so the chip then
     shows what the body holds now (hostile 56 and the page check). The
     frame runs no script, so in practice only a re-render changes it; the
     check makes that an invariant rather than an assumption.

   **Evaluating it** (`scripts/ctaeval`, private): `scripts/ctaeval
   <account> [N=300] > out.jsonl` runs the shipping heuristic over the
   account's own recent mail, so the weights can be tuned on real senders
   without anyone else's mail in a fixture.
   - *Reads.* The account's notmuch database only, read-only, as the server
     does (`NOTMUCH_CONFIG` from `~/.config/pneu/config.json`;
     `CTAEVAL_CONFIG` overrides the path): `notmuch search --output=messages
     --sort=newest-first` for `mimetype:text/html and not tag:sent and not
     tag:draft` (search.exclude_tags applies, so spam and trash are out),
     then `notmuch show` per message and the server's own body choice
     (`web.HTMLBody`, analyze), taking the first N whose rendered body is
     HTML. Nothing is tagged, synced, or read from the maildir directly
     (internal/testmail/cmd/ctaeval; its test checks the database revision
     is unchanged).
   - *Renders.* Each body through the shipping mailframe.js and actions.js
     in headless Chromium, a frame 900px wide, remote images off, cids
     unresolved. The JSON-LD capture runs too, so `declared` is what the
     app would see; the heuristic runs on every message, declared or not,
     so declared ones double as labelled examples.
   - *Network.* None, by four walls: the frame's CSP (no remote images,
     `default-src 'none'`); the page's own CSP (`'self'` only, which a
     srcdoc frame inherits); a DevTools request filter (`Fetch`) that
     fails every request not for the local server that serves the page
     and the scripts; and Chromium flags that resolve every
     name to nothing but 127.0.0.1 and send every other connection to a
     dead proxy (an IP literal needs no DNS). DevTools is spoken over a
     pipe, not a port, so no other local process can attach to a session
     rendering your mail. Chromium's own background requests (Google
     update and account endpoints) resolve to nothing too. Checked on the
     hostile corpus's tracker, receipt-pixel, CSS-url and LAN cases with a
     net log: no request left 127.0.0.1.
   - *Bodies never touch HTTP.* Each body is read by the node half and
     handed to the page over the DevTools pipe, as the argument of a
     `Runtime.callFunctionOn` of `evalOne` on the page's global object.
     The local server (`createServer` in scripts/ctaeval.mjs) serves only
     the page, `eval.js` and the three shipping scripts, has no route that
     reads the temp dir, and answers 403 to any Host but its own
     `127.0.0.1:<port>` (a DNS-rebinding page in the daily browser can
     reach the port but not name it). scripts/ctaeval.test.mjs checks
     both; the Go test runs it.
   - *Measured, 2026-09-30* (300 recent HTML bodies per account, judged
     from label and host only). As first built: 45 picks of 300, about
     40 right; most misses were real buttons tying within the margin
     ("View your order" vs "Download to track with Shop", eBay's two
     "Track package" links), and the wrong picks were verbless ("here",
     an image's `signature_…` alt). Tuned: a pick must lead with a verb;
     a tie between two verb-led candidates both over threshold goes to
     the earlier in the document; links with the same label count once;
     "web version", "view in web browser" and app-download promos are
     footer text. After: personal 63 picks (about 45 of 48 distinct
     right; the misses a marketing button in a receipt and a "see more"
     beating "track order"), work 20 picks plus 7 declared (GitHub), all
     right; most of the work account is colleagues' mail, where no pick
     is correct. No request left 127.0.0.1 in any run.
   - *Temp.* Bodies, the profile and the built helper go in a `mktemp -d`
     directory in `$TMPDIR` (0700, files 0600), removed on exit, on
     SIGINT/SIGTERM/SIGHUP and on an error. The processes are supervised
     so nothing outlives it: the bash wrapper runs each step (the build,
     the extractor, node) as a background job in its own process group
     (`set -m`) and waits for it; its trap sends SIGTERM to the whole
     group (the step and whatever it started: notmuch, the compiler),
     waits for the step (SIGKILL to the group after ten seconds), then
     for anything left in the group, before removing the dir. The
     extractor's notmuch runs take a context cancelled by SIGINT, SIGTERM
     or SIGHUP (`signal.NotifyContext`), so a signal to it alone kills and
     reaps the run it is waiting on; it then prints `ctaeval:
     interrupted` and exits 130. Node starts Chromium in its own process
     group and kills the group (SIGKILL) on exit, on a signal and on an
     uncaught error, then waits until the group is empty, since its
     helpers write to the profile. A SIGKILL to the wrapper can't be
     caught: it can leave the dir (`ctaeval.*`, still 0700) behind, and
     the running step with it. Tests: a SIGTERM to the script while
     Chromium runs, and while a stub notmuch (a shell with a sleeping
     child) runs; a SIGTERM to the extractor alone during a stub notmuch.
     Each checks exit 130, no survivor, no dir.
   - *Errors.* A category, never the error underneath: the extractor
     prints `ctaeval: notmuch search failed`, `notmuch show failed`,
     `cannot write a body`, `cannot create the output directory`,
     `cannot read the pneu config` or `no such account in the pneu
     config` (notmuch's stderr and the paths can name a message), and
     the browser half `ctaeval: the browser run failed` or why Chromium
     went.
   - *No bodies.* An account with no qualifying HTML writes `[]` as the
     manifest; the browser half then prints a zero summary and starts no
     browser.
   - *Prints.* JSONL on stdout, one line per message and only: `domain`
     (From's), `declared` (bool), `pick` (`{text, host}` or null),
     `runnerUp` (the best other destination, `{text, host, score}` or
     null), `score` (the pick's when there is one, else the best
     candidate's, so a "none" line still says how close it came), `top`
     (when nothing is picked, the best candidate as `{text, host}`, so a
     near miss can be judged; null otherwise), `candidates` (links that
     showed). Text is capped at 60 code points and URLs are cut to their
     host in the page, before anything leaves it. Then
     `{"summary": {messages, declared, picked, none, errors,
     blockedRequests}}`, where picked and none count messages without a
     declared action. A message that fails to render is counted in
     `errors` and never described.
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
visible, and its href unchanged. So is the highlight, so it never marks
something the pick no longer is.

As built (`mark`): a layer the size of the frame's viewport, positioned
over the frame inside its `.body` container, `overflow: hidden`, with a
dashed accent box at the anchor's rect. Being in the pane's flow, it
scrolls with the pane and is clipped by it and by the frame's box; the
sticky titles (z-index 1), the key bar (2) and dialogs (top layer) paint
above it, and it takes no pointer events. It follows the anchor on the
frame resizing, the window resizing and the frame scrolling sideways,
re-checking the guess each time (`stillPicked`), hides when the anchor
has no box or the guess no longer stands, and is shown only on the cursor
message (`article.message:not(.selected) .cta-mark { display: none }`).
While it shows it is re-validated every 250ms by a timer (a background
tab throttles it; it does nothing while the layer isn't displayed): if
the anchor's rect moved with no resize or scroll to follow, the anchor is
gone, or the guess no longer stands, it hides until the next of those
events redraws it. A timer rather than animation frames, which never come
under the harness's virtual time. The timer runs only while the highlight is
drawn and displayed: a re-validation that finds it hidden, or no longer
displayed, schedules nothing, and app.js calls the mark's `sync()` when
the cursor moves or a message expands or collapses, which stops the timer
or re-validates and restarts it; a thread replaced stops its marks, and a
frame repainted replaces its mark. Anything that throws while
re-validating hides it and schedules nothing more. CSS animations and transitions don't
get this far: an animated link is never picked. Colours: `--accent` and a
`color-mix` of it, nothing else.

## Link hints (`L`) (R8)

Labels are drawn in the app document over the frame, from each anchor's
rect intersected with the frame, the thread pane and the viewport;
anchors with nothing left are skipped. The overlay is clipped to the pane
and sits below dialogs. At most 200 labels, home-row alphabet. The
label → URL map is frozen when hints open; a resize, scroll, frame
replacement or cursor move closes them. Typing a label *selects*: the
status line shows the full destination (confirmation rules), and `Enter`
opens it through the browser-opened check (a mailto goes to the browser's
mail handler, as a body click on it does today; see *mailto* below).
`Esc` closes.

Decided in building it (2026-09-30):

- *Which links.* `a[href]` only (an `<area>`'s rect is its map's image),
  whose href, as the sanitizer left it, is `mailto:` or an absolute
  http(s) URL; at most 2000 anchors considered, 200 labelled, in document
  order. An http(s) link the browser-open check refuses (an IP literal,
  the app itself) still gets a label, and selecting it shows the refusal
  and the address; `Enter` opens nothing. A link over 4096 code points,
  http(s) or mailto, is refused the same way, since it can't be shown in
  full; a link that silently got no label would read as no link at all.
- *Visible.* The first client rect with area that survives frame ∩ pane ∩
  viewport (above the key bar, below the split's sticky title); its
  centre must hit the anchor or something inside it in the frame
  (`elementFromPoint`, which also skips `visibility:hidden` and
  `pointer-events:none`) and hit the frame itself in the app document (so
  nothing of ours covers it); and the anchor's opacity, multiplied up its
  ancestors, must be at least 0.5, as for the heuristic.
- *Labels.* All the same length over `asdfghjkl` (1 up to 9 links, 2 up to
  81, 3 beyond), so none is a prefix of another. Typing narrows (the rest
  hide); a complete label selects; a letter after a selection starts a
  new label; `Backspace` steps back; the arrows, PageUp/PageDown and
  Home/End scroll the status line (below); anything else is dropped,
  repeats and every modifier included: Shift+Enter opens nothing. Every key
  is the session's, from the frame too, and the page never scrolls under
  it.
- *Placement.* Each label's whole box, measured once drawn, lies inside
  the clip: it sits at its link's visible top-left, moved in from any edge
  it would cross, so a link with a sliver showing still gets a label that
  can be read. A label that can't fit at all is not drawn, and its link
  gets none.
- *Closing.* A resize of the window or of the frame (compared with its
  size at open, so an image arriving closes them), any scroll (the
  pane's, the window's, the frame's; not the status line's), the frame
  being replaced (app.js cancels, and the frame's own `load` closes them
  too), the cursor moving, the thread re-rendering, or another flash.
- *Status line.* The prompt, then the selection: `⏎ opens <URL>` (or
  `writes to` for a mailto), the URL as it will open, its own isolated
  `<bdi>` with controls drawn, never cut; the line lifts off the key bar
  and wraps (scrolling past a few lines). A known redirector is marked.
  Its own scrolling (wheel, scrollbar, or the inspection keys above) is
  the one scroll that doesn't close hints.
- *Nothing over it.* The key bar, and the status line in it, stack above
  the labels (`#hints` is z-index 1, the key bar 2), and while a label is
  selected any label whose box meets the status line is hidden, the
  selected one included: the destination `Enter` opens is never
  obscured.
- *Owning the line.* While hints are up nothing else writes the status
  line: any other flash (a sync failure, a tag's result) ends the hint
  session first (`yieldStatus`), then shows. Deferring the flash instead
  would keep the destination up, but cancelling is the safer failure:
  `Enter` is never left armed for a destination no longer on screen.
- *mailto.* pneu has no compose-from-mailto route today; a body click on a
  mailto opens it in a new window, which the browser hands to its mail
  handler. `Enter` on a mailto hint does exactly that
  (`window.open(url, '_blank', 'noopener,noreferrer')`), so a hint never
  does more than the click it stands for. Opening it in pneu's compose is
  a later change, to both.

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
