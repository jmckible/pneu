# Screenshots

`scripts/screenshot` captures pneu showing the fictional fixture mail, so
screenshots for the README, videos and marketing never show a real inbox.

```sh
scripts/screenshot docs/screenshot.png /t/personal/000000000000000e
scripts/screenshot --theme ~/some-theme/theme.css --size 1600x900 shot.png / /search?q=cabin
scripts/screenshot --unread 'id:os-08-kai@northwind.example' shot.png /t/work/000000000000000e
```

It builds the fixture's two notmuch databases in a throwaway directory, stubs
the lieer files so both accounts count as ready, runs a pneu server there on
port 7318 with `testdata/fakegmi` in place of lieer, and drives headless
Chromium over the DevTools protocol (`scripts/screenshot.mjs`): it opens a
session through the launch nonce, loads each path, and captures it at 2× by
default. Nothing touches your install: token, nonce, config, theme and
control socket all live in the throwaway directory, which is removed
afterwards.

- **Paths.** `/` is the inbox. `/t/<account>/<thread>` opens a thread; at a
  width past the split it shows beside the inbox. An empty search
  (`/search?q=subject:none`) shows the empty-list mark. Thread ids come from
  the fixture: `NOTMUCH_CONFIG=<dir>/config/personal/notmuch-config notmuch
  search tag:inbox`, with the directory from
  `go run ./internal/testmail/cmd/fixture-server -dir <dir>`.
- **Theme.** The app's colours come from `theme.css`, not the system's dark
  mode. The default is yours; `--theme` takes any file `install/pneu-theme`
  wrote, so one run per Omarchy theme gives a set.
- **Unread.** The fixture's inbox is mostly unread, because tests want
  that; real inboxes are mostly read. `--unread QUERY` marks every message
  outside the query read before the server starts.
- **The README image** is `docs/screenshot.webp`: the thread
  `/t/work/000000000000000e` ("November offsite venue", eight messages over
  two days) at the defaults, with `--unread 'id:20260923151204.m4g1c@mail.quillmate.example
  or id:HVC-esc-4471-2@northwind.example or id:os-08-kai@northwind.example'`,
  converted with `magick shot.png -quality 90 docs/screenshot.webp`. Its
  day labels are relative to the day it was taken.
- **The site's images** (pneu.mckible.com, `themes/` in the pneu-site repo)
  are the same thread and query per Omarchy theme: `<theme>-app.webp` at
  `--size 1600x937 --scale 1.25`, `<theme>-app-s.webp` that resized to 900
  wide, and `<theme>-narrow.webp`, the inbox `/` at `--size 512x846 --scale
  1.25`. A theme's `theme.css` comes from running `install/pneu-theme` with
  `HOME` pointed at a directory holding that theme's `colors.toml` under
  `.local/state/omarchy/current/theme/`.

## The fake mail

The mail is `testdata/mail`: Robin Hale's two accounts, `personal`
(robin@hale.example) and `work` (robin@northwind.example), as lieer-shaped
maildirs plus `tags.json`, the labels lieer would have applied (see
`testdata/testdata.go`). Every person, address and company in it is invented;
keep it that way.

For more material (a fuller inbox, a particular thread for a video), add
messages there. Tests assert on this fixture, so run `go test ./...` after
adding and update the tests that count it. If marketing needs diverge from
what tests want, give the showcase its own fixture directory rather than
bending the test one. The hostile corpus (`testdata/hostile`) is for the
sanitizer only; it isn't in the fixture mailboxes.
