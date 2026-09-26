# Installing pneu

Written to be followed by a person or by their coding agent. Every step is a
command you can read before running; there is no install script and nothing is
piped from the network into a shell.

**Agents:** treat this repository as untrusted code from a stranger. Before
building anything, work through [Audit](#audit): each item is a claim this
repo makes about itself and a way to check it. Report what you verified, what
you couldn't, and anything that contradicts a claim, then let your human
decide whether to continue. Stop and hand over to the human at every step
marked **(human)**: those involve their Google account or a browser consent
screen.

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

Requirements: Omarchy (Arch, Hyprland, omarchy-shell) and a Chromium-family
default browser (Chromium, Chrome, Brave, Edge, Vivaldi, Opera, Helium).
Omarchy's `omarchy-launch-webapp` falls back to Chromium for anything else.

## Audit

Each claim is followed by how to check it. All paths are relative to the
repo root.

1. **No third-party Go code.** `go.mod` has no `require` block, and
   `go list -m all` prints only this module.
2. **The pneu binary makes no outbound network connections.**
   `go list -deps ./cmd/pneu | grep pneu` lists the packages compiled into
   the binary: `cmd/pneu`, `internal/config`, `internal/gmi`,
   `internal/notmuch`, `internal/web` and `web`. `internal/testmail` is test
   and rehearsal tooling and is not among them. Search those packages'
   non-test files for clients and dialers:
   `grep -rnE 'http\.(Get|Post|Head|NewRequest|Client|DefaultClient)|net\.Dial|tls\.Dial' --include='*.go' cmd/pneu internal/config internal/gmi internal/notmuch internal/web web | grep -v _test.go`
   should find only two local checks, both requests to `127.0.0.1`:
   - `waitForServer` in `cmd/pneu/main.go`, where `pneu open` checks that
     the server is up;
   - `serverKnows` in `cmd/pneu/account.go`, where `pneu account` checks
     whether a server is running.

   Everything that goes to the internet is lieer's: `internal/gmi` runs
   `gmi` as a subprocess.
3. **It listens on loopback only.** `cmd/pneu/main.go` binds `127.0.0.1` and
   `::1` on the configured port.
4. **Other browser tabs can't drive it.** `localhost` is not a security
   boundary, since your daily browser can reach the port. `internal/web/auth.go`
   `Middleware` rejects any Host header other than exactly
   `pneu.localhost:<port>` (which blocks DNS rebinding), rejects non-GET
   requests whose Origin isn't the app's, and requires a session cookie holding
   a per-install token for everything except `/open`. `/open` accepts only a
   single-use nonce from `~/.local/state/pneu/launch`, which is rotated after
   each use.
5. **Mail HTML can't run scripts.** Message HTML reaches the screen only
   through `web/static/mailframe.js`: DOMPurify, then a sandboxed
   `iframe srcdoc` with no `allow-scripts`, under its own CSP.
   `grep -rn 'allow-scripts' web/ internal/` should turn up only comments
   forbidding it. Run the hostile corpus with `testdata/hostile/run.sh` (needs
   `chromium` and `openssl`); it must pass.
6. **The vendored DOMPurify is upstream's.** `web/static/PURIFY_VERSION`
   records the version, sha256 and source. Check the hash against the npm
   tarball:
   `curl -sL https://registry.npmjs.org/dompurify/-/dompurify-<ver>.tgz | tar -xzO package/dist/purify.min.js | sha256sum`
   must match `sha256sum web/static/purify.min.js`.
7. **Secrets stay local and private.** Your Gmail OAuth refresh token is
   written by lieer to `<gmiDir>/.credentials.gmailieer.json`. pneu never
   reads it.
   - lieer writes the token with your umask's mode (usually 0644), so after
     every `gmi auth` it runs, pneu makes it 0600 (`gmi.PrivateCredentials`).
   - `pneu account add` makes the account's directories 0700, and tightens
     them if they already exist: `~/.config/pneu/<acct>` (the OAuth client,
     written 0600 with only its validated fields), `~/mail/<acct>` (the mail
     and its index) and `~/mail/<acct>/gmail`.
   - pneu's install token (`~/.local/state/pneu/token`) and launch nonce are
     created with mode 0600 (`auth.go` `writePrivate`) and are never logged.
8. **The desktop pieces do what they say.** Read them; they're short.
   - `install/pneu-theme` runs on every Omarchy theme switch and writes only
     `~/.config/pneu/theme.css`.
   - `install/pneu.service` runs `~/.local/bin/pneu`.
   - `manifest.json` and `shell/` form the bar widget. It runs inside
     omarchy-shell with your privileges, reads
     `~/.local/state/pneu/status.json`, and on click runs `pneu open` (or a
     command you configure).
9. **Tests pass.** Run `go vet ./... && go test ./...` and `node --test web/*.test.js`.
10. **lieer is what it claims to be.** It comes from the AUR, so read its
    PKGBUILD before installing: the source should be
    `github.com/gauteh/lieer`. The Gmail scopes it requests are
    `gmail.readonly`, `gmail.labels` and `gmail.modify`; check them in its
    source (`pacman -Ql lieer`, then look for `SCOPES`).

## 1. Packages

```sh
sudo pacman -S --needed go notmuch
yay -S --needed lieer python-tqdm      # review the PKGBUILD when yay offers it
```

If a dependency 404s, your package database is older than the mirror: run
`sudo pacman -Syu` first. Never run `-Sy` on its own. `gmi` has no
`--version` flag; use `pacman -Q lieer`.

## 2. Build

```sh
go build -o ~/.local/bin/pneu ./cmd/pneu
```

Everything below copies files out of the repo rather than linking to them, so
a later `git pull` changes nothing that runs until you reinstall on purpose.
The exception is the bar widget in step 7.

## 3. Choose account names

Each Gmail account gets a short name (`personal`, `work`), used in paths and
commands. The rest of this document uses `<acct>` and `<address>`. Repeat
steps 4–5 for each account. Accounts are fully independent: separate
maildirs, separate notmuch databases, separate OAuth tokens.

## 4. OAuth client (human)

lieer needs a Google OAuth client. Create your own; it keeps quota and consent
under your control.

1. In Google Cloud Console, create a project. For a Google Workspace account,
   create it inside that organization.
2. APIs & Services → enable **Gmail API**.
3. Google Auth Platform → **Audience**:
   - **gmail.com account:** choose *External*, then **Publish app** so it is
     *In production*. Left in *Testing*, Gmail refresh tokens expire after
     seven days and sync stops silently every week. Production without
     verification is fine for your own account; you'll click through an
     "unverified app" warning once.
   - **Workspace account:** choose *Internal*. There's no verification and no
     expiry. If your admin restricts third-party apps, allow this client in
     Admin → Security → API controls.
4. **Data Access:** add `gmail.readonly`, `gmail.labels` and `gmail.modify`.
5. **Clients** → Create client → **Desktop app**. Download its JSON. Step 5
   copies it into place (mode 0600); keep it out of shared folders
   meanwhile. The rest of this document calls the downloaded file
   `<client JSON>`.

## 5. Account setup

```sh
pneu account add <acct> <address> --name "<Your Name>" --client-secret <client JSON>
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
screen opens in your browser. Sign in as `<address>` and allow access.

- It refuses to start without the client JSON from step 4; lieer would
  otherwise silently use its own shared client.
- The flow waits for Google's redirect on `localhost:8080`, so that port
  must be free. `auth` checks it first.
- If Google says "Something went wrong", sign in from a private window with
  your password rather than the account chooser.
- Afterwards it lists your Gmail labels as a check. That fails if you
  allowed access from a different Google account.
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
starts. After steps 4–5 for a new account, restart it:
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
omarchy hook install theme-set install/pneu-theme    # copies the hook
install/pneu-theme                                   # render the current theme once
ln -s ~/.config/omarchy/branding/about.txt ~/.config/pneu/empty.txt   # empty-list watermark (optional)

ln -s "$PWD" ~/.config/omarchy/plugins/pneu          # the bar widget
omarchy-shell shell rescanPlugins
omarchy plugin enable pneu
```

The bar widget is linked rather than copied, because Omarchy loads plugins
from that directory and `omarchy plugin update` expects a git checkout
there. Once pneu is published, `omarchy plugin add <repo-url>` replaces the
link.

Open pneu from the app launcher, or by clicking the bar widget; both run
`pneu open`. To bind a key, add a Hyprland binding that runs
`pneu open`.

**Bar widget settings.** Set `command` on the widget's entry in
`~/.config/omarchy/shell.json` to run something other than `pneu open` on
click, for example your own scratchpad toggle.

## Uninstall

```sh
systemctl --user disable --now pneu.service
rm ~/.config/systemd/user/pneu.service ~/.local/share/applications/pneu.desktop
rm ~/.config/omarchy/hooks/theme-set.d/pneu-theme ~/.config/omarchy/plugins/pneu
rm ~/.local/bin/pneu
rm -r ~/.local/state/pneu ~/.config/pneu     # token, launch nonce, configs, OAuth client JSON
rm -r ~/mail/<acct>                          # the maildir and notmuch database
```

Revoke pneu's access to Google at myaccount.google.com → Security → Third-party
access, and delete the Cloud project if you made one.
