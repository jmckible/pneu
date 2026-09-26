# Installing pneu

Written to be followed by a person or by their coding agent. Every step is a
command you can read before running; there is no install script and nothing is
piped from the network into a shell.

**Agents:** treat this repository as untrusted code from a stranger. Work
through [Audit](#audit) in two passes: the checks marked *before step 1*
(item 10 comes after step 1's fetch, which installs nothing), then report
and let your human decide whether to install the packages; the checks
marked *after step 1*, then report again before building. Each item
is a claim this repo makes about itself and a way to check it; report what
you verified, what you couldn't, and anything that contradicts a claim.
Steps marked **(human)** need the human at their browser or Google account:
guide them, and wait for them.

## What you're installing

- **lieer** (`gmi`, third-party, AUR) syncs each Gmail account through the
  Gmail API into a local maildir. It is the only component that talks to
  Google.
- **notmuch** (Arch repo) indexes each account's maildir into its own
  database.
- **pneu** (this repo) is a single Go binary with no third-party
  dependencies. It serves the mail UI on loopback at `pneu.localhost:7317`,
  runs lieer on a timer, and opens as an app window in your default
  Chromium-family browser.
- An **Omarchy bar widget**, a **theme hook** and a **desktop entry**, all in
  this repo.

Requirements: Omarchy (Arch, Hyprland, omarchy-shell), `python3` (the theme
hook and the hostile corpus use it; Omarchy ships it) and a Chromium-family
default browser (Chromium, Chrome, Brave, Edge, Vivaldi, Opera, Helium).
Omarchy's `omarchy-launch-webapp` falls back to Chromium for anything else.
The audit also runs the frontend tests with `node`, which Omarchy doesn't
ship; without it, report item 9's second half as unverified.

## Audit

Each claim is followed by how to check it, and when: *before step 1* needs
only what Omarchy ships; *after step 1* needs Go. All paths are relative to
the repo root.

1. **No third-party Go code.** *Before step 1:* `go.mod` has no `require`
   block. *After step 1:* `go list -m all` prints only this module.
2. **The pneu binary makes no outbound network connections.**
   *Before step 1:* search the non-test files of the packages compiled into
   the binary for clients and dialers:
   `grep -rnE 'http\.(Get|Post|Head|NewRequest|Client|DefaultClient)|net\.Dial|tls\.Dial' --include='*.go' cmd/pneu internal/config internal/gmi internal/notmuch internal/web web | grep -v _test.go`
   should find only two local checks, both requests to `127.0.0.1`:
   - `waitForServer` in `cmd/pneu/main.go`, where `pneu open` checks that
     the server is up;
   - `serverKnows` in `cmd/pneu/account.go`, where `pneu account` checks
     whether a server is running.

   A wider search also finds `https://mail.google.com/…` in
   `internal/web/mail.go`: a link the app renders, never fetched.
   Everything that goes to the internet is lieer's: `internal/gmi` runs
   `gmi` as a subprocess.

   *After step 1:* `go list -deps ./cmd/pneu | grep pneu` confirms those are
   the packages compiled in: `cmd/pneu`, `internal/config`, `internal/gmi`,
   `internal/notmuch`, `internal/web` and `web`. `internal/testmail` is test
   and rehearsal tooling and is not among them.
3. **It listens on loopback only.** *Before step 1:* by default
   `cmd/pneu/main.go` binds `127.0.0.1` and `::1` on the configured port. The
   `-listen` flag is the only way to bind anything else, and
   `install/pneu.service` doesn't pass it. Separately, `pneu account auth`
   opens and closes `localhost:8080` once to check it's free for lieer's
   consent redirect (`CheckAuthPort` in `internal/gmi/onboard.go`).
4. **Other browser tabs can't drive it.** *Before step 1:* `localhost` is
   not a security boundary, since your daily browser can reach the port.
   `internal/web/auth.go` `Middleware` rejects any Host header other than
   exactly `pneu.localhost:<port>` (which blocks DNS rebinding), rejects
   non-GET requests whose Origin isn't the app's, and requires a session
   cookie holding a per-install token for everything except `/open`. `/open`
   accepts only a single-use nonce from `~/.local/state/pneu/launch`, which is
   rotated after each use.
5. **Mail HTML can't run scripts.** *Before step 1:* message HTML reaches
   the screen only through `web/static/mailframe.js`: DOMPurify, then a
   sandboxed `iframe srcdoc` with no `allow-scripts`, under its own CSP.
   `grep -rn 'allow-scripts' web/ internal/` should turn up only comments
   forbidding it. Run the hostile corpus with `testdata/hostile/run.sh`
   (needs `chromium`, `openssl` and `python3`); it must pass. It runs
   headless Chromium with `--no-sandbox`, against pages it serves itself on
   `127.0.0.1`.
6. **The vendored DOMPurify is upstream's.** *Before step 1:*
   `web/static/PURIFY_VERSION` records the version, sha256 and source. Check
   the hash against the npm tarball:
   `curl -sL https://registry.npmjs.org/dompurify/-/dompurify-<ver>.tgz | tar -xzO package/dist/purify.min.js | sha256sum`
   must match `sha256sum web/static/purify.min.js`.
7. **Secrets stay local and private.** *Before step 1, by reading; on disk
   after step 5.* Your Gmail OAuth refresh token is written by lieer to
   `<gmiDir>/.credentials.gmailieer.json`. pneu never reads it.
   - lieer writes the token with your umask's mode (usually 0644), so after
     every `gmi auth` it runs, pneu makes it 0600 (`gmi.PrivateCredentials`).
   - `pneu account add` makes the account's directories 0700, and tightens
     them if they already exist: `~/.config/pneu/<acct>` (the OAuth client,
     written 0600 with only its validated fields), `~/mail/<acct>` (the mail
     and its index) and `~/mail/<acct>/gmail`.
   - pneu's install token (`~/.local/state/pneu/token`, created by
     `auth.go` `LoadOrCreateToken`) and launch nonce (`writePrivate`) are
     mode 0600 and are never logged.
8. **The install pieces do what they say.** *Before step 1:* read them;
   they're short.
   - `install/packages` is step 1's one root command. It installs
     packages from the Arch repos only, builds lieer as you (never as
     root) from the AUR files item 10 reviews, installs that, and removes
     the build-only packages it added.
   - `install/pneu-theme` runs on every Omarchy theme switch and writes only
     `~/.config/pneu/theme.css` and the launcher icon
     (`~/.local/share/icons/hicolor/scalable/apps/pneu.svg`), in the theme's colours.
   - `install/pneu.service` runs `~/.local/bin/pneu`.
   - `manifest.json` and `shell/` form the bar widget. It runs inside
     omarchy-shell with your privileges, reads
     `~/.local/state/pneu/status.json`, and on click runs `pneu open` (or a
     command you configure).
9. **Tests pass.** *After step 1:* `go vet ./... && go test ./...` and
   `node --test web/*.test.js`.
10. **lieer is what it claims to be.** *After step 1's fetch, before its
    install:* lieer comes from the AUR, and the fetch leaves exactly the
    files that get built in `~/.cache/pneu/lieer`. Read `PKGBUILD`: its
    `source` should be a `github.com/gauteh/lieer` release tarball, with no
    `install=` script, and `build()`, `check()` and `package()` should do
    nothing but build, test and copy. The fetch has already checked the
    tarball against `sha512sums` and unpacked it:
    `grep -A4 SCOPES ~/.cache/pneu/lieer/src/lieer-*/lieer/remote.py` should
    show exactly `gmail.readonly`, `gmail.labels` and `gmail.modify`. Every
    `depends` and `makedepends` in `.SRCINFO` must be an Arch repo package
    (`pacman -Si`); `install/packages` refuses anything else. Report the
    commit you reviewed: `git -C ~/.cache/pneu/lieer log -1`.

## 1. Packages

**Fetch** lieer's AUR files and its source, as you. This installs
nothing; audit item 10 reviews what it leaves.

```sh
git clone https://aur.archlinux.org/lieer.git ~/.cache/pneu/lieer
makepkg --nobuild --nodeps --dir ~/.cache/pneu/lieer   # downloads the source, checks its sha512, unpacks it
```

**Install**, after the human's go-ahead: one command, one password dialog.

```sh
pkexec "$PWD/install/packages" "$USER" ~/.cache/pneu/lieer
```

Root goes through `pkexec`, not `sudo`: it opens Omarchy's password dialog on
the desktop, which names `install/packages`, and the human authorises it
there. An agent runs the command itself and never sees the password; `sudo`
would wait on a terminal the agent doesn't have. Tell the human before
running it: **exactly one dialog**, naming `install/packages`; any other
password dialog is not from this install. The command waits until they
answer, then installs go, notmuch, python-tqdm and lieer's dependencies,
builds and installs lieer, and removes the build-only packages it added.

If a dependency 404s, your package database is older than the mirror: the
human decides whether to run `pkexec pacman -Syu` first. Never run `-Sy` on
its own. `gmi` has no `--version` flag; use `pacman -Q lieer`.

Now run the audit's *after step 1* checks and report before building.

## 2. Build

```sh
go build -o ~/.local/bin/pneu ./cmd/pneu
```

Everything below copies files out of the repo rather than linking to them, so
a later `git pull` changes nothing that runs until you reinstall on purpose.
The exception is the bar widget in step 7.

## 3. The account

Ask the human for one thing: the Gmail address (`<address>`). Work out the
rest, then show all four values and wait for the human to confirm or
correct them:

- **Kind:** `gmail.com` or `googlemail.com` is a personal account; any other
  domain is Google Workspace. Step 4 differs between them.
- **Account name** (`<acct>`), a short name used in paths and commands:
  `personal` for a personal account, otherwise the domain's first label
  (`work@acme.com` → `acme`).
- **Display name** (`<Your Name>`): the full name in
  `getent passwd "$USER"`, else `git config --global user.name`.

Repeat steps 3–5 for each account. Accounts are fully independent: separate
maildirs, separate notmuch databases, separate OAuth tokens.

## 4. OAuth client (human)

lieer needs a Google OAuth client, and each person makes their own, so quota
and consent stay under their control. This is the one long human step.

**Agents:** walk the human through it one screen at a time. Open each link
with `setsid -f xdg-open '<url>'` (plain `xdg-open` can block until the
browser exits), say what to do on that screen, and wait for "done" before
the next one. Every link carries `authuser=<address>` so the Console opens
as the right Google account, and after the first screen
`project=<project ID>`.

1. **Project:** `https://console.cloud.google.com/projectcreate?authuser=<address>`.
   Name it `pneu`; for a Workspace account, pick that organization as its
   location. Ask for "done" *and* the project ID, which may differ from the
   name: once the project exists, it's the `project=` value in the browser's
   address bar.
2. **Gmail API:**
   `https://console.cloud.google.com/apis/library/gmail.googleapis.com?authuser=<address>&project=<project ID>`
   → **Enable**.
3. **Get started:**
   `https://console.cloud.google.com/auth/overview?authuser=<address>&project=<project ID>`.
   **Get started** opens a four-part form:
   - **App information:** app name `pneu`, user support email `<address>`.
   - **Audience:** *External* for a personal account. *Internal* for a
     Workspace account: there's no verification and no expiry. If the admin
     restricts third-party apps, allow this client in Admin → Security →
     API controls.
   - **Contact information:** `<address>`.
   - **Finish:** agree to the Google API Services User Data Policy, then
     **Create**.
4. **Publish** (personal accounts only):
   `https://console.cloud.google.com/auth/audience?authuser=<address>&project=<project ID>`
   → **Publish app**, so it is *In production*. Left in *Testing*, Gmail
   refresh tokens expire after seven days and sync stops silently every
   week. Production without verification is fine for your own account; the
   consent screen shows an "unverified app" warning once.
5. **Client:**
   `https://console.cloud.google.com/auth/clients/create?authuser=<address>&project=<project ID>`
   → application type **Desktop app** → **Create** → **Download JSON**.

The scope list (*Data Access*) can stay empty: lieer asks for its three
scopes on the consent screen in step 5.

The download is the newest `~/Downloads/client_secret_*.json`; find it
there rather than asking the human for the path, and use that literal path
for both of step 5's commands. The rest of this document
calls it `<client JSON>`. The browser saves it world-readable; step 5
copies it into place (mode 0600) and deletes the download.

## 5. Account setup

```sh
pneu account add <acct> <address> --name "<Your Name>" --client-secret <client JSON>
rm <client JSON>                     # pneu keeps its own 0600 copy
pneu account auth <acct>
```

`pneu account add` lays out the account and says what it did at each step.
Rerunning it finishes an interrupted run and changes nothing else. It:

- **Copies the client.** It checks `<client JSON>` is a Desktop-app client
  and copies it to `~/.config/pneu/<acct>/client_secret.json` (mode 0600).
  It never prints the client.
- **Writes the notmuch config** `~/.config/pneu/<acct>/notmuch-config`,
  which indexes `~/mail/<acct>`. These settings aren't notmuch's defaults,
  and each one matters:
  - `new.tags` is empty so a stray `notmuch new` never marks old mail as
    unread in the inbox.
  - `new.ignore` keeps lieer's state files out of the index.
  - `search.exclude_tags=spam;trash`.
  - `maildir.synchronize_flags=false` makes tags the source of truth. lieer
    renames files itself.
  - `user.*` lets `notmuch reply` recognise every address you own. The new
    address also goes into the other accounts' `user.other_email`.
- **Creates the empty database** with `notmuch new`.
- **Initialises lieer** in `~/mail/<acct>/gmail`:
  `gmi init --no-auth --replace-slash-with-dot <address>`.
  `--replace-slash-with-dot` can't be changed later without a full re-pull.
- **Makes lieer ignore `pneu-touch`,** pneu's internal marker for tags
  written during a full pull: `gmi set --ignore-tags-local`, keeping any
  tags already listed. lieer must never push it.
- **Adds the account** to `~/.config/pneu/config.json`.

**(human)** `pneu account auth` runs lieer's consent flow: Google's consent
screen opens in your browser. Sign in as `<address>` and allow access. An
agent runs the command itself and leaves it waiting; the consent screen is
the human's part.

- A personal account's consent screen may first say Google hasn't verified
  the app: **Advanced** → **Go to pneu (unsafe)**. It's your own client.
- It refuses to start without the client JSON from step 4; lieer would
  otherwise silently use its own shared client.
- The flow waits for Google's redirect on `localhost:8080`, so that port
  must be free. `auth` checks it first.
- If Google says "Something went wrong", sign in from a private window with
  your password rather than the account chooser.
- Afterwards it checks that Gmail answers for `<address>` ("Authorized:
  Gmail answers for …"). That fails if you allowed access from a different
  Google account.
- pneu never reads the token lieer stores.

Every manual `gmi` run for an account goes through `pneu gmi <acct> …`,
which runs in the account's directory, against its notmuch database,
under the same lock the server uses.

## 6. Service

```sh
install -Dm644 install/pneu.service ~/.config/systemd/user/pneu.service
systemctl --user daemon-reload
systemctl --user enable --now pneu.service
```

**Adding an account later.** The server reads its accounts only when it
starts. After steps 3–5 for a new account, restart it:
`systemctl --user restart pneu.service`. `pneu account add` reminds you
when a server is running.

The server downloads each new account's mail itself; there is no separate
first-pull step.

- **Newest first.** lieer stores the newest mail first, so the inbox is
  readable a few minutes in. The rest of the archive follows, which takes
  hours on a large mailbox and as much disk as the mail itself.
- **Progress.** The app shows it with how far back the mail is complete.
  The bar widget shows the percentage, and `pneu account status` prints it
  in a terminal.
- **Read-only until done.** Until the first download finishes, an account
  can be read but not changed: archive, star, mark-read and send wait for
  it.
- **Interruptions.** If the download is interrupted, whether by a restart,
  a crash or a network stall of ten minutes, the server resumes it without
  downloading anything twice.
- **Re-auth.** When Gmail access later expires or is revoked, the app offers
  Reconnect, which runs the same consent flow as `pneu account auth`.
  An app left in *Testing* in step 4 expires after seven days.

## 7. Desktop

```sh
install -Dm644 install/pneu.desktop ~/.local/share/applications/pneu.desktop
install -Dm644 -t ~/.local/share/pneu brand/mark.svg    # the hook colours the launcher icon from it
omarchy hook install theme-set install/pneu-theme    # copies the hook
install/pneu-theme                                   # render the current theme and icon once

ln -s "$PWD" ~/.config/omarchy/plugins/pneu          # the bar widget
omarchy-shell shell rescanPlugins                    # returns before the rescan ends
timeout 30 sh -c 'until omarchy plugin list | grep -q "^pneu "; do sleep 1; done'
omarchy plugin enable pneu
pneu open
```

`pneu open` should bring up a window titled "Inbox · Pneu". The browser may
print warnings of its own to the terminal after `pneu open` returns; they
aren't pneu's, and its exit status is what counts.

The bar widget is linked rather than copied, because Omarchy loads plugins
from that directory and `omarchy plugin update` expects a git checkout
there. So link the checkout you mean to keep: every later change to it
goes live in the bar. Once pneu is published, `omarchy plugin add
<repo-url>` replaces the link.

Open pneu from the app launcher, or by clicking the bar widget; both run
`pneu open`. To bind a key, add a Hyprland binding that runs
`pneu open`.

**Bar widget settings.** Both go on the widget's entry in
`~/.config/omarchy/shell.json`, e.g. `{ "id": "pneu", "style": "Minimal" }`.

- `style`: `Default` always shows the mark: in the bar's text colour when
  the inbox is read, in the accent colour with the unread count otherwise.
  `Minimal` shows the mark only while there is unread mail, in the accent
  colour and without a count, and takes no space in the bar otherwise.
  Either style turns the mark the bar's urgent colour when sync is failing
  or the server is down (Minimal shows up for it too), and the tooltip says
  what's wrong.
- `command`: run something other than `pneu open` on click, for example
  your own scratchpad toggle.

## Uninstall

**Agents:** run this section as you ran the install, and report what you
removed, what the human chose to keep, and what the check at the end found.

Stopping the service interrupts a first download that's still running;
mail kept after that is partial.

```sh
omarchy plugin remove pneu --yes                     # turns the widget off, unlinks it, rescans
systemctl --user disable --now pneu.service
rm ~/.config/systemd/user/pneu.service
systemctl --user daemon-reload
rm ~/.local/share/applications/pneu.desktop ~/.config/omarchy/hooks/theme-set.d/pneu-theme ~/.local/bin/pneu
rm -r ~/.local/share/pneu ~/.local/share/icons/hicolor/scalable/apps/pneu.svg
jq -r '.accounts[].name' ~/.config/pneu/config.json   # the accounts, listed before their config goes
```

**(human)** Each account's downloaded mail and its index are in
`~/mail/<acct>`. Gmail still has all of it, but downloading it again takes
hours on a large mailbox: ask before deleting it.

```sh
rm -r ~/mail/<acct>                                  # each account listed above
rmdir --ignore-fail-on-non-empty ~/mail
rm -r ~/.config/pneu ~/.local/state/pneu             # token, launch nonce, configs, OAuth client
rm -rf ~/.cache/pneu                                 # lieer's AUR files and build
```

**Packages** are the human's call, one by one: other software may use Go or
notmuch.

```sh
pkexec pacman -Rns --noconfirm lieer python-tqdm
pkexec pacman -Rns --noconfirm notmuch
rm -rf ~/.cache/go-build                             # with Go: the build cache step 2 filled
pkexec pacman -Rns --noconfirm go
```

**(human)** Google keeps two things. Open each link with
`setsid -f xdg-open '<url>'`, as in step 4:

- **The access you granted:**
  `https://myaccount.google.com/connections?authuser=<address>` → **pneu**
  → **Delete all connections**.
- **The Cloud project:**
  `https://console.cloud.google.com/cloud-resource-manager?authuser=<address>`
  → select the pneu project → **Delete**. Keep it to reinstall later: the
  same project can issue a new Desktop client.

The browser keeps `pneu.localhost`'s cookie and site data; clear them there
if you like.

**Check.** Each of these should come back empty or say the thing doesn't
exist:

- `systemctl --user cat pneu.service`
- `omarchy plugin list | grep pneu` and `grep -n pneu ~/.config/omarchy/shell.json`
- `ss -ltn | grep ':7317 '`
- `find ~/.config ~/.local ~/.cache -iname '*pneu*'`, apart from this
  checkout and anything named after its path (Claude Code's per-project
  caches, for instance)
- `ls ~/Downloads/client_secret_*.json`, in case step 5's `rm` didn't run
- `pacman -Q go notmuch lieer python-tqdm` lists only what the human kept.
