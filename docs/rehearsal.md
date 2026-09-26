# Rehearsing INSTALL.md

Three levels of dress rehearsal for installing pneu, from cheapest to most
real. Only level 1 is built.

## 1. The sandbox harness: `scripts/rehearse`

This runs INSTALL.md itself, not a copy of it: `rehearse script` turns the
numbered steps' shell blocks and file blocks into a script, and the harness
runs that script. A new placeholder or a block the harness can't place
fails `go test ./internal/testmail/cmd/rehearse`. A full run from scratch
takes about three seconds.

```sh
scripts/rehearse                 # run steps 1–7 for one account, then check the result
scripts/rehearse --fail          # the same through a killed first pull and a revoked token
scripts/rehearse --serve         # the same, then serve it to your browser until Ctrl-C
scripts/rehearse shell           # a sandbox shell, for a person or an agent to follow INSTALL.md
scripts/rehearse shell --serve   # the same, with the browser bridge up for rehearse-serve
```

### What the sandbox isolates

- **Home directory.** `HOME` is a fresh directory, `home/` inside the
  sandbox. It is seeded with the pieces of an Omarchy install that
  INSTALL.md touches, copied from this machine.
- **The sandbox itself.** By default it is a new `mktemp -d` directory
  (`/tmp/pneu-rehearsal.XXXXXX`). A `run` that passes removes it; a failing
  one keeps it for inspection.
  - `REHEARSE_DIR` picks the directory instead. After resolving `..` and
    symlinks, it must be strictly inside `/tmp`, `$TMPDIR` or
    `$XDG_RUNTIME_DIR`.
  - An existing `REHEARSE_DIR` is deleted only if it holds the
    `.pneu-rehearsal` marker the harness writes; otherwise it must be
    empty.
  - `shell --keep` needs `REHEARSE_DIR`.
- **Environment.** The environment is emptied. `GOCACHE` and `GOMODCACHE`
  still point at the real ones, so builds stay fast.
- **Network.** The sandbox runs in its own network namespace, which has only
  a loopback interface. That's how port 7317 is handled: the sandbox's pneu
  listens on 7317 exactly as INSTALL.md and `install/pneu.service` say, and
  nothing inside can reach the real server, the internet, or anything else
  on this machine's localhost.
- **User.** A nested user namespace maps you back to your own uid, so files
  stay yours and nothing runs as root.
- **lieer.** `gmi` is `stubgmi` (`internal/testmail/cmd/stubgmi`), first on
  PATH. See its package comment for the knobs. `STUBGMI_COUNT` defaults to
  2000 here. `STUBGMI_DURATION=60s` makes the first pull slow enough to
  watch. `STUBGMI_FAIL=token|stall|kill` reproduces the failures the app
  has to handle.
- **Commands that reach the real session** are stubs that log instead:
  `systemctl` (the user manager ignores the sandbox HOME and would act on the
  live `pneu.service`), `systemd-run`, `journalctl`, `omarchy`,
  `omarchy-shell`, `omarchy-launch-or-focus-webapp`, `omarchy-launch-webapp`,
  `notify-send`, `hyprctl`, `xdg-open`, `loginctl`, `sudo`, `pkexec` and
  `yay`.
  - `systemctl` is logged, but for `pneu.service` it also does in the sandbox
    what the real manager would. `enable --now` or `start` runs the unit's
    `ExecStart` under its `Environment=PATH`, with `%h` expanded and the
    stubs put first on PATH. `stop` stops it. Everything after step 6 runs
    against a live server, exactly as on a real machine.
  - `pacman -Q…` queries run for real, because they only read. Any other
    `pacman` command is logged.
  - `git clone` of an AUR repo copies `testdata/aur/<name>` (lieer's real
    AUR files, at the commit in its `COMMIT`); every other `git` is real.
    `makepkg` is logged, and step 1's `pkexec install/packages` with it.
  - `omarchy` does in the sandbox what touches only files under HOME:
    `plugin list` lists the plugins directory (step 7 waits on it after
    the rescan), `plugin remove` unlinks, and `hook install` copies.
    Everything else it only logs.
  - `systemd-run` runs its command in the foreground instead of starting a
    unit, and `journalctl -u` prints that output.
  - When the run ends, the harness prints what each stub would have done.

### The human steps

- **Step 4, the OAuth client.** Replaced by `rehearse-human-oauth <acct>`.
  It "downloads" a Desktop-app client JSON of the right shape to
  `~/Downloads/client_secret_<acct>.json`, which is `$CLIENT_JSON`, the
  value of INSTALL.md's `<client JSON>`.
- **The consent screen.** By default `stubgmi` grants consent at once. With
  `STUBGMI_AUTH=browser` (as in `--fail`) it binds `localhost:8080`, prints
  a Google consent URL, and waits, like `run_local_server`. Allowing access
  is what Google then does: redirect to `localhost:8080` with the URL's
  `state` and a code. `rehearse grant URL` does exactly that, and
  `BROWSER=rehearse-browser` plays the person who clicks Allow at the
  terminal.

### What `run` checks after step 7

1. **Session.** `pneu open` gives a URL, and following the `/open` redirect
   gives a session.
2. **First pull.** It waits for the server's own first pull to finish,
   printing its progress as `pneu account status` shows it.
3. **Result.** It confirms:
   - the inbox renders;
   - the messages are indexed;
   - the next sync went through stubgmi;
   - `status.json` and `theme.css` were written.
4. **Uninstall.** Without `--serve`, it then runs INSTALL.md's Uninstall
   section (`rehearse script --uninstall`) and fails if the server is still
   running or anything named pneu, or `~/mail`, is left in the sandbox's
   HOME.

With `--fail`, stubgmi runs with `STUBGMI_FAIL=kill,token`. The run then
checks, in order:

1. **The kill.** The first pull dies halfway, leaving a message in
   `mail/tmp`. The account shows needs-pull with one failure.
2. **Read-only.** An archive on the account is refused (409).
3. **Retry now.** `POST /accounts/<acct>/pull` starts a pull that clears
   `mail/tmp`, resumes, and completes.
4. **Re-auth.** The next sync hits `invalid_grant` and the account turns to
   reauth. Reconnect (`POST /accounts/<acct>/reauth`) returns Google's
   consent URL; granting it reconnects the account, and it syncs again.

### Opening the sandbox in a browser

Start with `--serve`, or run `rehearse-serve` inside a `shell --serve`. The
sandbox is then reachable at `http://rehearse.localhost:7417/open?nonce=…`.
Add `STUBGMI_DURATION=90s` to watch the first pull's strip and meter fill
in.
Change the port with `REHEARSE_PORT`.

- **How it works.** `rehearse bridge-out` runs outside the namespace and
  relays to `bridge-in` inside it over a unix socket. On the way it rewrites
  `Host` and a same-origin `Origin` to `pneu.localhost:7317`, and refuses any
  other Host.
- **Why a different hostname.** Cookies aren't port-scoped. On
  `pneu.localhost` the sandbox's session cookie would replace the real
  pneu's, which uses the same cookie name.
- **The token stays out of sight.** The printed URL holds only the
  single-use nonce, never the install token.

### What level 1 can't show

- Real systemd semantics: `Restart=`, `TimeoutStopSec=`, login start.
- Real lieer and Google: OAuth screens, quotas, a mailbox of real size.
- Package installation.
- The Hyprland window and the bar widget.
- Omarchy's hook runner.

## 2. A second Linux user (not built)

Real systemd and a real login session, still no Google.

```sh
sudo useradd -m pneutest
sudo machinectl shell pneutest@     # a full login: its own systemd --user manager
```

As `pneutest`:

- **Clone and go.** Clone the repo and put `stubgmi` at `~/.local/bin/gmi`.
  The unit's `PATH=%h/.local/bin:…` then picks it ahead of `/usr/bin/gmi`.
  Then follow INSTALL.md with a real `systemctl --user enable --now`.
- **The one deviation is the port.** Both users share one network, so set
  `"port"` in that user's `config.json` to anything but 7317.
- **Browser.** Open the app in a separate browser profile, or the cookie
  clash described in level 1 applies.

The Omarchy steps fail there without a desktop session. That's expected, and
it's the part level 3 covers.

What this proves beyond level 1:

- The unit starts at login and restarts on failure.
- A slow `STUBGMI_DURATION` pull behaves under a real `systemctl stop`.
- `pneu gmi` waits correctly on the lock held by a running service.

## 3. A clean Omarchy VM (not built)

The real thing, end to end, with nothing prepared.

1. **Snapshot.** Take a snapshot of a fresh Omarchy VM.
2. **Account.** Use a throwaway Gmail account with a few hundred messages,
   and a Google Cloud project created for it.
3. **Agent.** Start a fresh coding agent in the VM, told only "install pneu
   from this repo".
4. **Repeat.** Roll back to the snapshot between attempts.

What to watch for:

- **The audit.** Does the agent audit before building, and report what it
  couldn't verify?
- **Handoffs.** Does it stop at every (human) step, including the Cloud
  console and the consent screen?
- **Getting stuck.** Where does it get stuck or improvise?
- **Time to a readable inbox.**
- **Real-world behaviour.** Does the real first pull behave as
  docs/onboarding.md predicts: newest first, and the progress output it
  parses?
