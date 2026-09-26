// Command stubgmi simulates lieer 1.6 (`gmi`) with no network: a rehearsal
// installs it first on PATH as `gmi`, so INSTALL.md and pneu run against it
// exactly as they would against the real thing. It keeps lieer's files
// (.gmailieer.json, .state.gmailieer.json, the resume file, .lock), its
// command line, its output byte for byte in format, and its failure modes.
// The mailbox behind it is testdata/mail, multiplied.
//
// It is not testdata/fakegmi, which internal/gmi's tests drive by contract.
//
// Knobs (environment):
//
//	STUBGMI_COUNT=N        messages in the mailbox; fixtures are copied, each
//	                       copy dated one fixture-span older (default: the fixtures)
//	STUBGMI_FIXTURE=all    personal, work or all
//	STUBGMI_DURATION=60s   wall time a full pull spends listing and downloading
//	STUBGMI_NOISE=1        lieer's "remote: reducing batch request size" lines
//	                       between batches (0 turns them off)
//	STUBGMI_FAIL=mode,...  failures, each fired once per repository (recorded in
//	                       .stubgmi.json; STUBGMI_FAIL_REPEAT=1 fires it every run):
//	                         token  the refresh token is revoked the first time
//	                                an already-pulled repository authenticates;
//	                                every run then fails with lieer's
//	                                invalid_grant traceback until `auth -f`
//	                         stall  output stops partway through content and the
//	                                process never exits (SIGINT still ends it)
//	                         kill   partway through content, a message is left in
//	                                mail/tmp and the process SIGKILLs itself
//	STUBGMI_FAIL_AT=0.5    fraction of the content phase where stall/kill fire
//	STUBGMI_AUTH=instant   instant: `auth` prints the consent URL and succeeds
//	                       at once. browser: lieer's run_local_server behaviour —
//	                       bind localhost:8080 (fail if taken), open the URL
//	                       with $BROWSER (fail with no browser, as webbrowser
//	                       does), print it, and wait for one request. The URL
//	                       is Google's, which the stub can't serve: consent
//	                       is the redirect Google would send, a GET of its
//	                       redirect_uri with its state and any code
//	                       (http://localhost:8080/?state=S&code=C)
//	STUBGMI_SLOW_BATCH=3s  the first content batch takes this long, printing
//	                       nothing but reading all the while, as a batch of
//	                       big messages over a slow link does
//	STUBGMI_LOG=path       append start/end lines per invocation, like fakegmi
//
// A command that needs credentials and has none would start lieer's consent
// flow; under STUBGMI_AUTH=instant the stub prints the URL and exits 1 instead
// of granting access nobody gave.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type gmi struct {
	action  string
	args    map[string]string // parsed options; flags map to "1"
	multi   map[string][]string
	pos     []string
	quiet   bool
	verbose bool
	local   *local
	box     *mailbox
}

// interrupted is set by SIGINT; lieer gets KeyboardInterrupt, which unwinds
// its `with notmuch2.Database()` blocks, so the stub finishes the batch in hand.
var interrupted atomic.Bool

func main() {
	logInvocation("start")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	go func() { <-sig; interrupted.Store(true) }()

	g := &gmi{}
	err := g.run(os.Args[1:])
	logInvocation("end")
	var pe *pyError
	switch {
	case err == nil:
	case errors.Is(err, errNoConsent):
		os.Exit(1)
	case errors.As(err, &pe):
		pe.print()
		os.Exit(1)
	case errors.Is(err, errInterrupt):
		os.Stderr.WriteString("Traceback (most recent call last):\n  File \"/usr/bin/gmi\", line 23, in <module>\n    g.main ()\nKeyboardInterrupt\n")
		signal.Reset(syscall.SIGINT)
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(time.Second)
		os.Exit(130)
	default:
		var ue usageErr
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, "usage: gmi [-h] {pull,push,send,sync,auth,init,set} ...")
			fmt.Fprintln(os.Stderr, "gmi: error:", ue.msg)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "stubgmi:", err)
		os.Exit(1)
	}
}

var (
	errInterrupt = errors.New("interrupted")
	// errNoConsent ends a run that would have waited on a consent nobody gives.
	errNoConsent = errors.New("no credentials")
)

type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

func logInvocation(what string) {
	p := os.Getenv("STUBGMI_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	wd, _ := os.Getwd()
	fmt.Fprintf(f, "%s\t%d\t%s\t%s\t%s\n", what, time.Now().UnixNano(), strings.Join(os.Args[1:], " "), wd, os.Getenv("NOTMUCH_CONFIG"))
}

// option specs per subcommand, from gmailieer.py:Gmailieer.main. The value
// is the canonical name; a leading '=' marks an option that takes a value.
var common = map[string]string{
	"-C": "=path", "--path": "=path", "-c": "=credentials", "--credentials": "=credentials",
	"-s": "no_progress", "--no-progress": "no_progress", "-q": "quiet", "--quiet": "quiet",
	"-v": "verbose", "--verbose": "verbose",
}

var specs = map[string]map[string]string{
	"pull": {"-t": "list_labels", "--list-labels": "list_labels", "--limit": "=limit", "-d": "dry_run", "--dry-run": "dry_run",
		"-f": "force", "--force": "force", "-r": "resume", "--resume": "resume"},
	"push": {"--limit": "=limit", "-d": "dry_run", "--dry-run": "dry_run", "-f": "force", "--force": "force"},
	"send": {"-d": "dry_run", "--dry-run": "dry_run", "-i": "i3", "-t": "read_recipients", "--read-recipients": "read_recipients", "-f": "=i1"},
	"sync": {"--limit": "=limit", "-d": "dry_run", "--dry-run": "dry_run", "-f": "force", "--force": "force", "-r": "resume", "--resume": "resume"},
	"auth": {"-f": "force", "--force": "force", "--auth-host-name": "=auth_host_name", "--auth-host-port": "*auth_host_port",
		"--noauth_local_webserver": "noauth_local_webserver"},
	"init": {"--replace-slash-with-dot": "replace_slash_with_dot", "--no-auth": "no_auth"},
	"set": {"-t": "=timeout", "--timeout": "=timeout", "--replace-slash-with-dot": "replace_slash_with_dot",
		"--no-replace-slash-with-dot": "no_replace_slash_with_dot", "--drop-non-existing-labels": "drop_non_existing_labels",
		"--no-drop-non-existing-labels": "no_drop_non_existing_labels", "--ignore-empty-history": "ignore_empty_history",
		"--no-ignore-empty-history": "no_ignore_empty_history", "--ignore-tags-local": "=ignore_tags_local",
		"--ignore-tags-remote": "=ignore_tags_remote", "--file-extension": "=file_extension",
		"--remove-local-messages": "remove_local_messages", "--no-remove-local-messages": "no_remove_local_messages",
		"--local-trash-tag": "=local_trash_tag", "--translation-list-overlay": "=translation_list_overlay"},
}

var actions = []string{"pull", "push", "send", "sync", "auth", "init", "set"}

func (g *gmi) parse(argv []string) error {
	// sendmail compatibility: gmailieer.py strips -oi and -i from argv.
	var clean []string
	for _, a := range argv {
		if a != "-oi" && a != "-i" {
			clean = append(clean, a)
		}
	}
	argv = clean
	if len(argv) == 0 {
		return usageErr{"the following arguments are required: action"}
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Println("usage: gmi [-h] {pull,push,send,sync,auth,init,set} ...")
		os.Exit(0)
	}
	g.action = argv[0]
	spec, ok := specs[g.action]
	if !ok {
		return usageErr{fmt.Sprintf("argument action: invalid choice: %s (choose from %s)", pyRepr(g.action), strings.Join(actions, ", "))}
	}
	g.args, g.multi = map[string]string{}, map[string][]string{}
	var unknown []string
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, hasVal = strings.Cut(a, "=")
		}
		canon, ok := spec[name]
		if !ok {
			canon, ok = common[name]
		}
		switch {
		case !ok && strings.HasPrefix(a, "-") && len(a) > 1:
			unknown = append(unknown, a)
		case !ok:
			g.pos = append(g.pos, a)
		case strings.HasPrefix(canon, "="):
			if !hasVal {
				if i+1 >= len(argv) {
					return usageErr{fmt.Sprintf("argument %s: expected one argument", name)}
				}
				i++
				val = argv[i]
			}
			g.args[canon[1:]] = val
		case strings.HasPrefix(canon, "*"):
			for i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				g.multi[canon[1:]] = append(g.multi[canon[1:]], argv[i])
			}
		default:
			g.args[canon] = "1"
		}
	}
	switch {
	case g.action == "init" && len(g.pos) == 0:
		return usageErr{"the following arguments are required: account"}
	case g.action == "init" && len(g.pos) > 1, g.action != "init" && g.action != "send" && len(g.pos) > 0:
		unknown = append(unknown, g.pos...)
	}
	if len(unknown) > 0 {
		return usageErr{"unrecognized arguments: " + strings.Join(unknown, " ")}
	}
	g.quiet = g.args["quiet"] != ""
	g.verbose = g.args["verbose"] != ""
	return nil
}

func (g *gmi) run(argv []string) error {
	if err := g.parse(argv); err != nil {
		return err
	}
	// Gmailieer.setup: -C changes directory (init creates it first).
	if p := g.args["path"]; p != "" {
		g.vprint("path: %s", p)
		if g.action == "init" {
			os.MkdirAll(p, 0o777)
		}
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			fmt.Printf("error: %s is not a valid path!\n", p)
			return raise("NotADirectoryError: error: "+p+" is not a valid path!",
				lieerFrame("gmailieer.py", 462, "setup", "raise NotADirectoryError(\"error: %s is not a valid path!\" % args.path)"))
		}
		if err := os.Chdir(p); err != nil {
			return err
		}
	}
	if g.args["dry_run"] != "" {
		fmt.Println("dry-run: ", "True")
	}
	switch g.action {
	case "init":
		return g.initialize()
	case "auth":
		fmt.Println("authorizing..")
		if err := g.loadRepository(false); err != nil {
			return err
		}
		return g.authorize(g.args["force"] != "")
	case "set":
		return g.set()
	}
	// pull, push, sync, send: setup(load=True), then remote.get_labels().
	if err := g.loadRepository(g.action == "send"); err != nil {
		return err
	}
	if err := g.getLabels(); err != nil {
		return err
	}
	if g.args["dry_run"] != "" {
		return nil // the stub doesn't simulate dry runs beyond this
	}
	switch g.action {
	case "pull":
		if g.args["list_labels"] != "" {
			for _, l := range g.box.labels(g.local.cfg.ReplaceSlashWithDot) {
				fmt.Printf("%-30s %s\n", l[0], l[1])
			}
			return nil
		}
		return g.pull()
	case "push":
		return g.push()
	case "sync":
		if err := g.push(); err != nil {
			return err
		}
		return g.pull()
	case "send":
		return g.send()
	}
	return nil
}

// envInt, envFloat and envDur read knobs, with defaults.
func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func envDur(k string) time.Duration {
	d, _ := time.ParseDuration(os.Getenv(k))
	return d
}

// sleep paces the simulation; SIGINT cuts it short.
func sleep(d time.Duration) error {
	end := time.Now().Add(d)
	for {
		if interrupted.Load() {
			return errInterrupt
		}
		left := time.Until(end)
		if left <= 0 {
			return nil
		}
		time.Sleep(min(left, 50*time.Millisecond))
	}
}

// failMode reports whether mode, one of STUBGMI_FAIL's comma-separated
// modes, should fire now, and records that it did.
func failMode(mode string) bool {
	if !slices.Contains(strings.Split(os.Getenv("STUBGMI_FAIL"), ","), mode) {
		return false
	}
	if os.Getenv("STUBGMI_FAIL_REPEAT") == "1" {
		return true
	}
	b, _ := os.ReadFile(stubFile)
	if strings.Contains(string(b), `"`+mode+`"`) {
		return false
	}
	var fired []any
	for _, m := range strings.Split(strings.Trim(strings.TrimPrefix(strings.TrimSpace(string(b)), `{"fired": `), "[]}"), ", ") {
		if m = strings.Trim(m, `"`); m != "" {
			fired = append(fired, m)
		}
	}
	fired = append(fired, mode)
	os.WriteFile(stubFile, []byte(pyJSON([]kv{{"fired", fired}})), 0o644)
	return true
}
