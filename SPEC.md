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
  All, each with its key number), search on the right, sync glyph far right.
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
  opens without being asked: archiving or trashing the open thread (`e`,
  `t`, `#`, from either pane) moves the cursor to the next row and opens it,
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
  active pane's title only). Body text is `--fg` at 14px; only dates, counts,
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
  (flashes, undo hint, sync) on the right. It swaps with focus in the split.
- **Sync state.** One glyph at the header's far right: an accent braille
  spinner while any account's sync runs, empty when idle (a locked state
  isn't distinguished yet). Never a progress bar. The server broadcasts
  `syncing` when a sync starts and `sync` when it ends, failures included;
  only an end that pulled something re-renders the list, without moving the
  cursor. `R` asks for a sync on every account now (`POST /sync`, coalesced
  with one already running) and flashes "Syncing…" in the footer. Launching
  the window (`/open` with the nonce) and coming back to it (focus or shown
  again, not a click into a mail frame; at most every 20s) ask the same,
  quietly: the phone buzzes on Gmail's push, and an idle sync is about a
  second, so the mail is there by the time you look. *default*
- **Theme.** Everything references a CSS variable; nothing hardcodes a color.
  Verify against three Omarchy themes (one light) before merge. *default*

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
- **Actions** (`e` `#` `!` `s`) apply to the whole thread and return to the
  list at the same cursor position.
- **Reply** is an inline box under the last message: `r` opens it with the
  quote prefilled, `Ctrl+Enter` sends, `Esc` discards with confirmation if
  non-empty. Sending pulls, and the sent message appears in the thread.
- **Spam** is `+spam -inbox` only. Gmail's classifier takes the signal; there
  is no block. Use `Open in Gmail` for a filter. (Not built yet: the tag
  endpoint has no spam action or undo inverse.)
- **Trash from the Spam view** is refused client-side because lieer allows
  only one of inbox/spam/trash; supporting it needs a trash variant that also
  drops `spam`, with undo restoring it.

## Compose page

Reached by `c`. Plain text. Fields: from (account picker, defaults to the
account of the current view's last-opened thread), to, cc, subject, body.
Same send/discard keys as inline reply. *default*
