# pneu UI spec

Decisions from the 2026-09-24 design pass. Colors come from the desktop theme
(`/theme.css`, from the Omarchy theme's colors.toml); this file decides everything else.
Items marked *default* were taken as best practice without discussion — change
them here if they're wrong.

## System

- **Type.** The desktop's mono font everywhere, including mail chrome. Two
  sizes: body (list rows, message text) and small (dates, counts, meta). No
  bold except unread rows; emphasis is color, not weight. HTML bodies are
  the exception: their sans text renders in Inter, which goes in ahead of
  Arial/Helvetica/`sans-serif` (Liberation Sans here, soft at a fractional
  device scale). Serif and mono stacks are left as sent.
- **Layout.** Header: view nav on the left (Inbox · Starred · Sent · Spam · Trash ·
  All, each with its key number), search on the right, the sync status line far right.
  Two named panes, the list and the thread, and exactly one is active: it
  gets the keys, its title row is accent-colored, and the footer lists its
  keys. At `140ch` and above, with a thread open, they sit side by side:
  the list takes 40% of the window (clamped to `64ch`-`100ch`), the thread
  the rest (at least `48ch`), each with a one-line sticky title row (the
  list's is view and row count, the thread's is its subject). With no
  thread open the list fills the window. The title's color is the
  indicator; nothing shifts when focus moves. Below `140ch` the active pane
  is the whole screen (list or thread; the list's title row hides, the nav
  already names the view).
  Opening is explicit: `j/k` on the list only move the cursor. `Enter`,
  `o`, `l`, `Tab` or a click opens the cursor row (in the pane when split,
  as its own page when narrow), marks it read, and gives it the keys.
  `u`/`Esc`/`h` in the thread give the keys back to the list and leave the
  pane open; `Esc` on the list closes the pane (the list fills the window
  again). `Tab`/`Shift+Tab` switch panes, except inside a mail body, where
  Tab walks its links. `+` maximizes the active pane (the narrow layout)
  until `+` again or the window crosses `140ch`. The one time a thread
  opens without being asked: archiving, trashing or spamming the open
  thread (`e`, `t`, `#`, `!`, from either pane) moves the cursor to the next row and opens it,
  keys staying put; an emptied list closes the pane.
  The URL is always the open thread's, or the list's when none is open, so
  a reload or narrow window shows the same thing; a thread URL loaded
  directly gets its list beside it with the cursor on its row. An open
  pushes a history entry, so Back returns to the list; the pane moving on by
  itself (the next thread after a removal, closing) replaces it. The pane
  without the keys dims its cursor rail. Clicking inside a pane gives it
  the keys.
- **Contrast.** The theme hook supplies only `--bg`, `--fg`, `--accent`,
  `--selection`. Every other tone is derived in app.css with `color-mix` from
  those two ends, so a theme with a dim comment color can't dim the app:
  `--muted` is fg 65%, `--border` is fg 12%, `--unread` is fg pushed toward
  white 40%, `--accent-hot` is the accent with chroma pushed up 35% (for the
  active pane's title and a stale or failing sync status line only). Body text is `--fg` at 14px; only dates, counts,
  and to/cc lines are muted. Authors are never muted. *default*
- **Separation.** Rows are divided by a 1px `--border` line, not zebra
  stripes. Message cards are bordered with a 3px left rail. Section headers
  (thread subject, compose fields) use a single rule above, never boxes
  inside boxes. *default*
- **Keyboard-first.** A cursor row is always present in index views and is
  the primary visual state. Mouse works everywhere but nothing is mouse-only.
- **Keys (HEY navigation, Gmail actions).**
  Views: `1` Inbox · `2` Starred · `3` Sent · `4` Spam · `5` Trash · `6` All.
  Move: `j/k` (or `↓/↑`) rows (list) or scroll three lines (thread) · `g/G` first
  and last row · `>`/`<` older and newer page (`<` lands on the last row) ·
  `n/p` messages · `Space`/`Shift+Space` page the thread · `Enter`/`o` open
  (list) or fold (thread) · `x` select · `u`/`Esc` back to list (Esc
  first blurs any input).
  Panes: `h` (`←`) list · `l` (`→`) thread · `Tab`/`Shift+Tab` switch · `Esc` (list) closes the thread · `+` maximize.
  Act: `e` archive · `t` or `#` trash · `!` spam · `s` star · `U` unread ·
  `z` undo · `r` reply · `a` reply all · `w` or `c` write · `v` open in
  Gmail (forward, block, RSVP live there) · `R` sync now · `/` search ·
  `?` key overlay.
  Number keys work from anywhere, including inside a thread, so getting back
  to the inbox is `1` or `u`. The header nav fuses the number to each view
  name as a small accent superscript (btop-style: the key lives inside the
  label); the key footer and the `?` overlay render keys in accent too.
- **Actions in index views** act on the selection if any, else the cursor row.
  After an action the cursor stays put (next row slides into it). *default*
- **Undo.** Every destructive action shows a one-line toast with `z` to undo
  until the debounced push fires. *default*
- **Key footer.** A bar across the bottom lists the keys for the pane that
  has them (list, thread, or compose) on the left and holds the status line
  (flashes, undo hint) on the right. It swaps with focus in the split.
  It lists only the everyday keys (the rest are in `?`); the thread's adds
  `o link`, `f files` and `X unsubscribe` only while each has something to
  act on: a chip on the cursor message, a viewable attachment in the
  thread, a `List-Unsubscribe` header on the cursor message.
- **Sync state.** A short status line at the header's far right says how
  current the view is. In priority order: `A service worker was removed
  from this window · Reset window data (bar menu)` in `--accent-hot`, in
  either mode, when the page finds a service worker registered on its
  origin (pneu never registers one: `worker-src 'none'`); it unregisters
  each and holds the line until reloaded, since whatever the worker served
  may still be running (docs/client.md R4). Then, on a client (docs/client.md),
  `Can't reach <server> · retrying` in `--accent-hot` while its link to
  the server is down (`<server>` is the SSH target it was paired with;
  the page keeps what it shows, and the line comes back when the link
  does); `Checking…` with an accent
  braille spinner while a sync `R` asked for is queued or running, or `R`
  hasn't been answered yet (the server says queued at once; the line gives
  up waiting after 10s): scheduled, launch and focus syncs are quiet, or
  a 30s period would keep the line spinning; `<account>: sync failing`
  (`+N` for more) in `--accent-hot` once an account has failed as many
  times as the bar widget calls sick (one); `Updated 14m ago` in
  `--accent-hot` when idle and older than three sync periods; otherwise
  `Updated just now` / `Updated 3m ago`, muted. The age is the oldest
  account's last successful sync: the view is only as fresh as its
  stalest account. On a client, whatever the line says gets a muted `·
  Update available` after it while the version nudge stands (this
  machine's build and the server's differ; docs/client.md "Versions and
  updates"), and the details say which side is older and which bar-menu
  item updates it. Only ready accounts count; one in its first pull or
  waiting on setup or reconnection is the `#accounts` strip's (on a
  client, an account whose Gmail access lapsed shows `pneu account auth
  <name>` to run in a terminal there, and Fix with agent, instead of
  Reconnect: the consent waits on the server's lieer), and with
  no ready account synced yet the line is empty. A push (after a
  keystroke) never shows as checking either. The age refreshes every 30s while
  shown and on focus; a screen reader hears changes of state (a live
  region), not the age ticking. Never a progress bar.
  `data-accounts` carries each account's `lastSync`, `queued`, `running`,
  `failures` and `error` (and, for the details only, `pollEvery` and
  `push`), and the line reads its stale threshold from the
  sync period (`data-every`), so the first paint is right; SSE `account`
  keeps them current: sent when a sync is queued, starts and ends.
  `syncing`/`sync` still bracket each sync, for the line only; a sync that
  pulled something is also a `view` (below), which re-renders. Every
  `/events` stream opens with `hello`, carrying every account's view, so a
  reconnect puts the line right at once. A queued or running flag with no
  news for 11 minutes (its end lost while the stream stayed down; a sync
  is bounded at 10) is ignored.
  Clicking the line opens its details (Esc or a click closes it), and the
  `?` overlay leads with the same: per account, the last sync as a time
  and an age, whether it's checking now or queued, failures and the last
  error, and its instant mail (docs/push.md D7): `Instant: delivering ·
  last message 2m ago`, `Instant: listening · no message since pneu
  started`, `Instant: quiet · nothing in 24h`, `Instant: starting`, `Instant:
  off`, or `Instant: failing — <reason in words>` with the command that
  fixes it (`pneu account push <name>`; `pneu push init --reconsent` for
  the push owner's grant; for an API, permission or org policy,
  `pneu account push <name>` prints the fix; none for a network
  failure, which retries). Push never touches the line itself or turns
  anything red. The footer is the polling note, `Checks every 30s`, from
  the engine's real delay (`pollEvery`, gmi `PollDelay`: the ready
  accounts' shortest, else `data-every`), and an account backing off
  after failures says `checks every 4m while failing`. On a client also
  the link (connected, or why not, and since when) and both builds'
  revisions, and the commands are to run in a terminal there (they run
  on the server over SSH, as `pneu account auth` does).
  On a client a write that couldn't reach the server says so instead of
  its usual failure: `Not sent: can't reach <server>.` when nothing left
  this machine (pressing again is safe), `<server> didn't answer; checking
  when it's back.` when it may have landed; the next `hello` (the link
  back) brings the generation, and the list reconciles. An unsubscribe
  (X) whose POST never left says `Not sent: can't reach <server>. Nothing
  was sent.` with `y` to preview again; one whose answer was lost says
  `<server> didn't answer; checking when it's back.` and asks for the
  stored result as usual; a preview that couldn't reach the server
  flashes `Unsubscribe: Can't reach <server>.` A page loaded
  while the link is down is the client's own error page: what's wrong in
  local words, the bar-menu action or terminal command that fixes it, and
  a reload once the link is up. A server never shows any of this.
  `R` asks for a sync on every account now (`POST /sync`, coalesced with
  one already waiting). Opening the window asks too: `pneu open` sends
  `launch` over the control socket (docs/client.md) before it opens or
  focuses the window, so every path in (a new window, a restored one, a
  focus) syncs; `/open` itself starts nothing. Coming back to the window (focus or shown again,
  not a click into a mail frame; at most every 20s) asks the same. The
  phone buzzes on Gmail's push, and an idle sync is about a second, so
  the mail is there by the time you look. *default*
- **Other windows.** Two windows (or a stale tab) stay in step through the
  server's view generation (docs/client.md "Changes from other windows"):
  `(epoch, gen)`, the epoch random per server start, gen bumped once by
  every write that changes what a list or thread shows (a tag write, undo,
  mark-read, a send, a mailto unsubscribe, a sync that pulled something).
  Each bump is an SSE `view {epoch, gen, from, threads}`: `threads` the
  `(account, thread)` pairs written, none for a sync (anything may have
  changed); `from` the writing page's `X-Pneu-Window`, a random id per page
  load sent on every write. Every page is labeled with the pair as of
  before its query (`data-epoch`/`data-gen` on body), and every stream
  opens with `hello {epoch, gen, accounts}`, as of subscribing; no event
  the hello already counts follows it. On `view`, or a `hello` that
  differs from a pane's label, the list re-renders in place when quiet
  (the sync rule above: never under typing or a write in flight, cursor
  kept), and an open thread the event names (any, for a sync or a hello)
  is fetched again in place, keeping the cursor message, folds and
  scroll, without marking anything read. While you're in the middle of
  something there (a dialog, an unsubscribe, link hints, a text selection)
  its title says `changed elsewhere` instead, and it re-renders when
  you're done. A window skips only its own write's event, and only once
  it has applied that write's answer (the answer carries its gen); while
  its writes are in flight the event waits for them, and one never
  applied reconciles like any other. `from` saves a refresh, nothing more.
  An undo whose thread another window wrote since says so in its toast,
  `changed elsewhere · z undoes yours anyway`; z still undoes yours (last
  writer wins).
- **Theme.** Everything references a CSS variable; nothing hardcodes a color.
  Verify against three Omarchy themes (one light) before merge. *default*
- **Bar widget** (`shell/BarWidget.qml`, its decisions in `shell/status.js`).
  The mark in the bar's foreground at rest, the accent with the unread
  count, the urgent colour when the count can't be trusted; a first
  download shows its percent in place of the count; `style: Minimal` shows
  only unread mail or a warning. It reads only `status.json`: version 1
  (a server's) or 2 (a client's, docs/client.md). Warning: no readable
  file, this machine's pneu stopped or silent for 20 minutes, an account
  failing, needing re-auth or not set up, and on a client the server out
  of reach (`server.link` down, or `starting` for more than 90s) or
  silent (`server.statusAt` 20 minutes old: the counts are stale even
  though `updated` keeps ticking). A client still connecting just after
  start is neutral: `Connecting to <server>…`. The tooltip lists what's
  wrong: `Can't reach <server> since 14:02 · <server> is offline` (the
  reason in local words), `No word from <server> since 14:02`, the unread
  line with `(as of 14:02)` when stale, the accounts; on a client the
  commands that fix an account say `on <server>`. Every string from the
  file is drawn as plain text after control and bidi characters are
  dropped. Left click runs `pneu open` (or the `command` setting); right
  click opens the menu: **Open pneu**; **Fix with agent** only when
  there's something to fix (an account failing; on a client also the link
  out, a protocol mismatch, its own daemon stopped or silent, the server
  silent), which runs `pneu agent`; on a client **Update pneu** when
  `server.update` says this machine is older or the builds just differ
  (runs `pneu update` in Omarchy's floating terminal, which asks before
  changing anything) or **Update <server>** when the server is older
  (runs `pneu agent -update`, whose prompt says how to update it from the
  server's own checkout), neither a warning, the tooltip saying which;
  **Reopen pneu (reset done)** for 30
  minutes after a reset this widget started; **Reset window data…**,
  which first says what it does (closes pneu's app windows and deletes
  compose drafts saved in the browser; a pneu tab or popup needs the
  browser quit first) and runs `pneu reset-window` on a second click. Every item runs a fixed argv from `status.js`, never a shell
  line and never anything from the file. A suggested command shows an
  account name only when it is a plain word (`config.ValidName`), else
  `<account>`. *default*

## Index views

Inbox (default), Starred, Sent, Spam, Trash, All Mail. Same template, different
query. Paginated (`Newer`/`Older`, `<`/`>`). The split's title row says where
the page sits: `Inbox · 12` on a single page, `All Mail · 51–100 of 44,095`
when paged. The total is a thread count cached at each database's revision;
uncached, the page shows the range first and the total follows.

- **Merged stream.** Both accounts in one list sorted by date. No per-row
  account marker; the account shows on the thread page only.
- **One line per thread:** `authors · subject · count · date`. Authors and
  subject truncate; date is right-aligned and fixed width. The authors column
  scales with the viewport (`clamp(14ch, 22vw, 28ch)`). Count renders as a
  small bordered pill after the subject, only for threads with more than
  one message; on unread rows it takes an accent wash. A thread with an
  attachment (notmuch's index-time `attachment` tag, any message) shows a
  paperclip ahead of the date, in the date's muted tone.
- **Authors** show the account's own `user.name` (or address) as `me`, once,
  as Gmail does. Sent shows `To: …` instead: the recipients of the thread's
  newest sent message, `me` for the account's own address.
- **Row states:** unread (bold + accent), starred (glyph before authors),
  cursor (background), selected (checkbox glyph + background), all combinable.
- **Dates:** time if today, weekday if this week, `Mon D` this year, else
  `YYYY-MM-DD`. Full timestamp on hover. *default*
- **Star toggle** is the glyph; clickable and `s`.
- **Spam and Trash** add an `Empty` button in the header. Empty is confirmed
  once, then the messages are removed locally and pushed; Gmail purges.
  (Engine question: verify lieer pushes a local delete, otherwise Empty is
  `-spam`/`-trash` plus Gmail's 30-day purge.)
- **Search** is the same view with the query shown in the box.
- **Motion** (`web/static/motion.js`; the timings are the approved
  mock's). Small, quick, and never in the way: the state changes on the
  key and motion only catches up; the next key ends any row still
  opening or closing (and the cursor moving with it) and acts on the end
  state, while a glide is retargeted and a star plays out. With
  `prefers-reduced-motion` none of it runs: every change is what it was
  before (the cursor jumps; a removed row keeps its 120ms fade). Colours
  are the theme's tokens.
  - *The cursor glides.* One element under the rows carries the cursor
    row's look (selection background, accent rail, muted rail in the pane
    without the keys) and moves to the cursor row, measured (rows needn't
    share a height): 110ms, `cubic-bezier(.2,.7,.3,1)`, on `j/k`, `g/G`
    and a click. A press mid-glide retargets from where it is, so holding
    `j` never lags. A list rendered or re-rendered (a load, paging, a
    refresh, a resize) places it still. At rest it looks exactly as the
    row's own `.selected` look did.
  - *A removed row closes its gap.* Archive, trash or spam slides the row
    28px right as it fades, then its height closes (280ms in all); the
    rows below move up, so the next row moves into the cursor, which stays
    put. From the last row the cursor moves up in step. Undo is the
    reverse, shorter (240ms): the gap opens, the row fades in from the
    right.
  - *The star pops.* Starring (`s`, the thread pane's `s` on its row, an
    undo) scales the star 0 → 1.35 → 1 while it turns −30° → 8° → 0
    (360ms), six 3px accent sparks burst about 11px from it and fade
    (420ms after 70ms), and the subject slides over as the star's width
    opens (150ms). Unstarring shrinks it out (160ms). At rest the star is
    the same glyph as before. The thread page's stars (one per message)
    don't pop.

## Thread page

- **Header:** subject, account, thread count. Actions are keys; a small
  action strip repeats them for the mouse. `Open in Gmail` lives here.
- **Messages:** newest expanded, everything already read collapses to one
  line (`from · date`). `j/k` scroll the thread three lines and `Space`
  pages it (the thread holds focus while it has the keys, so arrows and
  PageDown work too); `n/p` move the message cursor; `Enter` folds or
  unfolds the cursor message. *default*
- **Message chrome:** flat, no indentation by depth. Each message is a card
  with a 3px left rail: border color at rest, `--fg` when unread, `--accent`
  under the cursor. Header shows `n of N` on the right.
- **Expanded message:** from, to/cc, date, then the body in the sandboxed
  frame (HTML) or a `<pre>` (text). Quoted text folds by default. Attachments
  as a list under the body.
- **Attachments.** Each is a link to its `/part` URL: images, PDFs, audio
  and video open in a tab, everything else downloads. What pneu can show
  gets a viewer: `f` opens the cursor message's first (else the thread's
  first), a plain click the one clicked (a modified click keeps the link's
  own behaviour). It is a
  near-fullscreen modal in the help overlay's dress: a title row with the
  name, `i of N` and Previous / Next (disabled at the ends) / Download /
  Open in tab / Close, the attachment below.
  `n`/`p` (`→`/`←`, unless a player has focus) step through every viewable
  attachment in the thread in document order; `d` downloads, `o` opens in a
  tab (only what `/part` serves inline), `Esc`/`q` close and give the keys
  back to the thread. Keys work from inside its frames too, except
  Chromium's PDF viewer, which keeps them: a PDF shows a hint, and a click
  anywhere in the dialog outside a control takes the keys back. The server
  decides the kind (`data-view`) from the declared type, or the filename
  when the type is generic (`application/octet-stream` and kin; a
  `text/plain` `.csv`/`.md`/`.ics` is refined too):
  - *image* (png, jpeg, gif, webp, avif, bmp, svg): an `<img>`, on a
    checkerboard. SVG only ever as an image.
  - *pdf*: Chromium's viewer in an `<iframe>`.
  - *video*, *audio*: the native player (`/part` answers ranges).
  - *text* (plain, json, logs, patches, source): a `<pre>`, decoded by the
    part's charset. Over 2 MB: a note and Download.
  - *csv* (csv, tsv): a table, first row as its header, at most 2000 rows,
    200 columns and 200,000 cells; a note says what was cut.
  - *markdown*: rendered (marked, GFM) into a mail frame at a reading
    width. *html*: the source rendered in a mail frame. Both are sanitized
    and sandboxed like a mail body, remote images behind a click.
  - *ics*: the first event's title, times, place, organizer and details. A
    TZID time shows as written with its zone named; a UTC time in local time.
  - *zip*: the archive's listing (name, size, modified), 1000 entries and
    256 KB of names at most. An archive of more than 20,000 entries or an
    8 MB central directory (read from its end record, zip64 too) isn't
    parsed: "Too many entries to list (N)" and Download. No per-entry
    download.
  - Anything else (docx, xlsx, heic…) only downloads. *default*
- **Drive files.** Docs, Sheets, Slides, Forms, Drawings, files and folders
  linked anywhere in the thread show as chips under the newest message's
  attachments (it is always expanded), once each in order of first mention,
  titled from the anchor text. The link is rebuilt from kind and file id with
  `authuser` set to the account.
- **Body colors.** Plain text renders in app colors. HTML bodies render in
  app colors too unless the sanitized document declares its own background
  or text color anywhere (inline style, `<style>`, `bgcolor`), in which case
  the frame keeps a light sheet: `#f2f2ef`, not pure white, inset with a
  `1ch` margin and a border so it reads as a document lying on the desk.
  mailframe.js decides per message and sets the frame's `html{}` rule and
  `color-scheme` accordingly; the theme's `--bg`/`--fg` are passed in as the
  frame's colors. This touches the sanitizer's prepended style, so it runs
  against the hostile corpus before merge.
- **Unsubscribe** (`X`) acts on the cursor message (or the one whose frame
  has the keys), from its `List-Unsubscribe` header only, never a body
  link: one-click (RFC 8058, DKIM-verified by pneu, posted by the server),
  else the sender's first mailto or web option. A confirmation shows the
  exact destination, or the whole message for a mailto, and `y` confirms.
  Design and security contract: docs/actions.md.
- **Primary link** (`o`): a message whose HTML declares exactly one
  schema.org JSON-LD action (GitHub's "View Pull Request" is the common
  one) gets a chip in its header, `o  <name> → <destination>`, the
  destination derived from the URL that opens (`(redirect)` for known
  click-trackers). `o` on the expanded cursor message, or from inside its
  frame, or a click on the chip opens it in the browser; the chip is the
  preview, so an `o` with it scrolled out of view brings it into view and
  a second `o` opens. The URL is checked again as it opens. Without a declared
  action, a button heuristic may guess one visible, button-like link: the
  chip is dashed and tagged `guess`, the link is outlined over the frame,
  and `o` opens it only if it is still there, visible and pointing where it
  did when picked. docs/actions.md.
- **Link hints** (`L`): labels over every visible http(s)/mailto link in the
  message's body, drawn by the app over the frame. Typing a label selects it
  and shows the full destination on the status line; `Enter` opens it (a
  mailto as a body click does), `Esc` closes; scrolling, resizing or moving
  the cursor closes them. docs/actions.md.
- **Actions** (`e` `#` `!` `s`) apply to the whole thread and return to the
  list at the same cursor position.
- **Reply** is an inline box under the last message: `r` opens it with the
  quote prefilled, `Ctrl+Enter` sends, `Esc` discards with confirmation if
  non-empty. Sending pulls, and the sent message appears in the thread.
- **Spam** is `+spam -inbox` only, undone by `-spam +inbox`. Gmail's
  classifier takes the signal; there is no block (the API has none, and a
  filter needs a scope lieer doesn't ask for). Use `Open in Gmail` to block.
- **Trash from the Spam view, and spam from the Trash view,** are refused
  client-side because lieer allows only one of inbox/spam/trash; supporting
  them needs variants that also drop the other tag, with undo restoring it.

## Compose page

Reached by `c`. Plain text. Fields: from (account picker, defaults to the
account of the current view's last-opened thread), to, cc, subject, body.
Same send/discard keys as inline reply. *default*

**Sending is never repeated.** A draft keeps one id from its first render
until it is sent, stored with the draft in the browser, so a resubmit, or
the same draft reopened after the window closed, is the same send. The
server writes each send's reservation to disk before handing the message
to lieer and its result after; a repeat is answered from that record.
What the page says when a send doesn't simply go out:

- **Not sent: …** — lieer failed before Gmail had the message (it never
  started, or failed before its send call). Fix and send again.
- **Sent, but the local copy failed** — Gmail has it; lieer couldn't store
  its copy. Never sent again; a pull follows.
- **May have been sent: check Sent in Gmail** — lieer failed at or after
  its send call (whatever error it reports) without showing that Gmail
  took it, or the server stopped mid-send. pneu won't send that draft again; to send it anyway, copy the
  text, discard the draft and start again.
- A draft already sent (or that may have been) and then edited is refused
  with the same advice.

On a client, a send the link fails gets the client's own page in place of
the form's answer, and the draft stays saved in the browser: `Not sent:
can't reach <server>` (nothing left this machine; go back and send again
once it's back), or `<server> didn't answer` (it may have been sent: check
Sent; sending the same draft again is answered from the server's record,
never sent twice). Its one button goes back to the draft; it never
reloads, which would post the form again.

If the browser can't save the draft before sending, the send is blocked
with a message: without the saved id a reopened draft could send twice.
A sent-but-unresolved draft keeps its id until discarded; records are kept
30 days from the last send attempt, and a never-sent draft older than that
gets a new id. A mailto unsubscribe follows the same rules: once sent (or
maybe sent), previewing it again never sends it again.

Known limit: lieer's HTTP library itself resends a send whose connection
dropped mid-request, so on a flaky network one send can, rarely, arrive
twice. That happens inside lieer, beyond what pneu can see.
