# Screenshots

`scripts/screenshot` captures pneu showing the fictional fixture mail, so
screenshots for the README, videos and marketing never show a real inbox.

```sh
scripts/screenshot docs/screenshot.png /t/personal/000000000000000e
scripts/screenshot --theme ~/some-theme/theme.css --size 1600x900 shot.png / /search?q=cabin
```

It builds the fixture's two notmuch databases in a throwaway directory, stubs
the lieer files so both accounts count as ready, runs a pneu server there on
port 7318 with `testdata/fakegmi` in place of lieer, and drives headless
Chromium over the DevTools protocol (`scripts/screenshot.mjs`): it opens a
session through the launch nonce, loads each path, and captures it at 2× by
default. Nothing touches your install: token, nonce, config and theme all live
in the throwaway directory, which is removed afterwards.

- **Paths.** `/` is the inbox. `/t/<account>/<thread>` opens a thread; at a
  width past the split it shows beside the inbox. An empty search
  (`/search?q=subject:none`) shows the empty-list mark. Thread ids come from
  the fixture: `NOTMUCH_CONFIG=<dir>/config/personal/notmuch-config notmuch
  search tag:inbox`, with the directory from
  `go run ./internal/testmail/cmd/fixture-server -dir <dir>`.
- **Theme.** The app's colours come from `theme.css`, not the system's dark
  mode. The default is yours; `--theme` takes any file `install/pneu-theme`
  wrote, so one run per Omarchy theme gives a set.
- **The README image** is `docs/screenshot.webp`: the thread
  `/t/personal/000000000000000e` ("Cabin weekend in October?") at the
  defaults, converted with `magick shot.png -quality 90 docs/screenshot.webp`.

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
