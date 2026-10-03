package main

// pneu push init and pneu account push [--off] (docs/push.md D2–D4): the
// CLI half of push sync. It runs the consents, provisions the push
// project, and writes state.json under push.lock; the daemon runs what
// state.json says, told over the control socket (push-reload) and
// watched there (push-state). Consent and every network call happen
// outside push.lock; each commit re-reads the state under it. Every
// Google failure is told in pneu's own words, keyed off its closed code
// (pushwords.go): nothing Google wrote reaches a terminal or a client.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/push/state"
	"github.com/jmckible/pneu/internal/remote"
	"github.com/jmckible/pneu/internal/web"
)

const pushUsage = `usage:
  pneu push init --project <id> --client-secret <file> [--replace] [--reconsent]
  pneu push init [--reconsent]            (again: the push project and client already set up)
On a client it runs on its server over SSH; the consent opens here.`

// pushOut is where push's words go, and how its consent reaches a
// browser: this terminal's, or a client's over its event stream.
type pushOut interface {
	say(format string, a ...any)
	ctx() context.Context
	// pushConsent shows c's consent screen to the person and returns
	// Google's callback query for it, held to remote.ParseCallback's
	// shape. It isn't trusted: the caller checks it with c.Callback.
	pushConsent(c *google.Consent) (url.Values, error)
}

// pushEnv is what the push commands touch; tests swap it.
type pushEnv struct {
	cfgPath string
	store   state.Store
	api     *google.API
	socket  string // the control socket; "" when there is none (sockErr)
	sockErr error
	now     func() time.Time
	out     pushOut
	// deliverWait bounds the wait for a pushed account to report
	// delivering; pollEvery paces its push-state questions.
	deliverWait time.Duration
	pollEvery   time.Duration
}

// The commands' bounds.
var (
	pushLockWait    = 30 * time.Second // for push.lock, held by another push command
	pushDeliverWait = 60 * time.Second
	reloadTries     = 3 // push-reload answered "mismatch": re-read and send again
)

// newPushEnv builds the real one; a variable so tests can replace it.
var newPushEnv = func(cfgPath string, out pushOut) (*pushEnv, error) {
	dir, err := web.StateDir()
	if err != nil {
		return nil, err
	}
	e := &pushEnv{cfgPath: cfgPath, store: state.Store{Dir: state.Dir(dir)}, api: google.New(google.Options{}),
		now: time.Now, out: out, deliverWait: pushDeliverWait, pollEvery: time.Second}
	e.socket, e.sockErr = control.SocketPath()
	return e, nil
}

// termPush is this terminal's pushOut. Its consent binds lieer's callback
// port on both loopbacks the way lieer does (listen), and opens the
// consent screen in the desktop's browser (open).
type termPush struct {
	w      io.Writer
	listen func() ([]net.Listener, error)
	open   func(string) error
}

func newTermPush() termPush {
	return termPush{w: os.Stdout, open: openURL,
		listen: func() ([]net.Listener, error) { return listenLoopback(gmi.AuthPort, bindExclusive) }}
}

func (t termPush) say(format string, a ...any) { fmt.Fprintf(t.w, format+"\n", a...) }
func (t termPush) ctx() context.Context        { return context.Background() }

// errConsentWaiting: the callback port is taken, or a connection to it is
// still closing.
var errConsentWaiting = fmt.Errorf("a consent is already waiting on localhost:%d (lieer's or pneu's), or one just finished; try again in a minute", gmi.AuthPort)

// pushDone is the local listener's answer to the callback it takes.
const pushDone = "pneu: Google answered. You can close this tab; the terminal shows the result.\n"

// pushConsent binds the callback port first, holding it until Google's
// answer comes (a lieer consent can't start meanwhile, and one already
// waiting refuses this), then opens the consent screen.
func (t termPush) pushConsent(c *google.Consent) (url.Values, error) {
	u := c.URL()
	if !gmi.ValidConsentURL(u) {
		return nil, errors.New("the consent URL isn't Google's; not opening it")
	}
	lns, err := t.listen()
	if errors.Is(err, errPortBusy) {
		return nil, errConsentWaiting
	}
	if err != nil {
		return nil, fmt.Errorf("Google's consent comes back to localhost:%d, and %w", gmi.AuthPort, err)
	}
	cb := startCallback(lns, pushDone)
	defer cb.close()
	cb.arm(c.State())
	fmt.Fprintf(t.w, "Opening Google's consent screen in your browser:\n  %s\n", u)
	if err := t.open(u); err != nil {
		fmt.Fprintf(t.w, "Couldn't open a browser (%v): open the URL above yourself.\n", err)
	}
	timer := time.NewTimer(consentWait)
	defer timer.Stop()
	select {
	case raw := <-cb.got:
		return remote.ParseCallback(raw)
	case <-timer.C:
		return nil, fmt.Errorf("consent wasn't given within %v; nothing changed", consentWait)
	}
}

// pushCmd is `pneu push init`: here, or on a client, on its server.
func pushCmd(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return usageError{pushUsage}
	}
	// The server's half of a client's command.
	if len(args) == 2 && args[1] == "--stdin" {
		return accountStdin(remote.PushInit, os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("pneu push init", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	project := fs.String("project", "", "the push project's ID (docs/push.md D1)")
	secret := fs.String("client-secret", "", "the push project's own Desktop-app OAuth client JSON")
	replace := fs.Bool("replace", false, "set up a different client or owner (every account's push off first)")
	reconsent := fs.Bool("reconsent", false, "ask the owner's consent again even if the stored grant works")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{pushUsage}
	}
	if *project != "" && !google.ValidProject(*project) {
		return usageError{fmt.Sprintf("bad --project %q: a GCP project ID (6–30 of a-z, 0-9 and -, starting with a letter)", *project)}
	}
	p := initParams{project: *project, replace: *replace, reconsent: *reconsent}
	if *secret != "" {
		b, err := readSecretFile(*secret)
		if err != nil {
			return err
		}
		c, err := state.ParseClient(b)
		if err != nil {
			return fmt.Errorf("%s: %w", *secret, err)
		}
		if *project != "" && c.Project != *project {
			return fmt.Errorf("%s is a client of project %s, not --project %s: the push client must be the push project's own", *secret, c.Project, *project)
		}
		p.client = cleanPushClient(c)
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil || ok {
		if err != nil {
			return err
		}
		return newRemoteEnv(target).run(remote.PushInit, remote.PushInitRequest{
			Project: p.project, ClientSecret: string(p.client), Replace: p.replace, Reconsent: p.reconsent})
	}
	e, err := newPushEnv(cfgPath, newTermPush())
	if err != nil {
		return err
	}
	return e.init(p)
}

// accountPush is `pneu account push <name> [--reconsent | --off]`: here,
// or on a client, on its server.
func accountPush(args []string) error {
	fs := flag.NewFlagSet("pneu account push", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	off := fs.Bool("off", false, "turn instant mail off for the account")
	reconsent := fs.Bool("reconsent", false, "ask the mailbox's consent again even if the stored grant works")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*off && *reconsent) {
		return usageError{accountUsage}
	}
	name := pos[0]
	if !config.ValidName(name) {
		return usageError{fmt.Sprintf("bad account name %q: %s", name, config.NameRule)}
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil || ok {
		if err != nil {
			return err
		}
		if *off {
			return newRemoteEnv(target).run(remote.PushOff, remote.PushOffRequest{Name: name})
		}
		return newRemoteEnv(target).run(remote.Push, remote.PushRequest{Name: name, Reconsent: *reconsent})
	}
	e, err := newPushEnv(cfgPath, newTermPush())
	if err != nil {
		return err
	}
	if *off {
		return e.off(name)
	}
	return e.on(name, *reconsent)
}

// cleanPushClient is the push client JSON as stored and sent: its three
// fields under "installed", nothing else.
func cleanPushClient(c state.Client) []byte {
	b, _ := json.Marshal(map[string]any{"installed": map[string]string{
		"client_id": c.ID, "client_secret": c.Secret, "project_id": c.Project}})
	return b
}

// lock takes push.lock, waiting up to pushLockWait.
func (e *pushEnv) lock() (*state.Locked, error) {
	ctx, cancel := context.WithTimeout(e.out.ctx(), pushLockWait)
	defer cancel()
	return e.store.Lock(ctx)
}

// consent runs one consent for kind and trades its code for tokens. The
// callback is checked here, against this consent's own state (constant
// time), and only its code is exchanged, with this consent's verifier and
// redirect.
func (e *pushEnv) consent(creds google.Credentials, kind google.Kind, hint string, px pushCtx) (google.Token, error) {
	c, err := google.NewConsent(creds, kind, hint)
	if err != nil {
		return google.Token{}, fmt.Errorf("can't start a consent: %w", err)
	}
	q, err := e.out.pushConsent(c)
	if err != nil {
		return google.Token{}, err
	}
	code, err := c.Callback(q)
	switch {
	case errors.Is(err, google.ErrDenied):
		return google.Token{}, errors.New("consent wasn't given: Google answered with a refusal (declined at the screen, or refused for this client or by an admin); nothing changed")
	case errors.Is(err, google.ErrState):
		return google.Token{}, errors.New("the answer that came back isn't this consent's; nothing changed")
	case err != nil:
		return google.Token{}, errors.New("Google's answer carried no usable code; nothing changed")
	}
	tok, err := e.api.Exchange(e.out.ctx(), c, code)
	if err != nil {
		return google.Token{}, px.explain(err, kind)
	}
	return tok, nil
}

// --- pneu push init ---------------------------------------------------

// initParams are `pneu push init`'s, from the command line or a client's
// request. client is the push client JSON (cleanPushClient's form), nil
// to keep the stored one; project "" keeps the stored project.
type initParams struct {
	project            string
	client             []byte
	replace, reconsent bool
}

// init sets the push project up (D4): the client JSON copied in and
// checked, the owner's consent (unless the stored grant works), the
// owner's sub pinned, the permission probe, and the commit.
func (e *pushEnv) init(p initParams) error {
	o := e.out
	ctx := o.ctx()
	cur, err := e.store.Load()
	have := err == nil
	if err != nil && !errors.Is(err, state.ErrNoState) {
		return err
	}
	var c state.Client
	switch {
	case p.client != nil:
		if c, err = state.ParseClient(p.client); err != nil {
			return err
		}
	case have:
		if c, err = e.store.LoadClient(); err != nil {
			return fmt.Errorf("the stored push client: %w", err)
		}
		if err := state.CheckClient(cur.File, c); err != nil {
			return err
		}
	default:
		return errors.New("the first pneu push init needs --project and --client-secret (the push project's ID and its Desktop-app client JSON)")
	}
	switch {
	case p.project == "" && !have:
		return errors.New("the first pneu push init needs --project: the push project's ID, which must be the client JSON's project_id")
	case p.project != "" && p.project != c.Project:
		return fmt.Errorf("the push client is project %s's, not --project %s: the push client must be the push project's own", c.Project, p.project)
	}
	if err := checkInit(cur.File, have, c, p.replace); err != nil {
		return err
	}
	px := pushCtx{project: c.Project}
	if have && !p.replace {
		px.owner = cur.Owner.Email
	}
	o.say("Push project %s, client %s", c.Project, c.ID)

	// The owner: the stored grant when it still works, else a consent.
	var (
		access, refresh string
		id              google.Identity
		granted         time.Time
	)
	sameClient := have && c.ID == cur.Client && c.Project == cur.Project
	if sameClient && !p.replace && !p.reconsent {
		tok, err := e.api.Refresh(ctx, c.Credentials(), google.Owner, cur.Owner.Refresh, cur.Owner.Sub)
		switch code := google.CodeOf(err); {
		case err == nil:
			access, refresh, granted = tok.Access, cur.Owner.Refresh, cur.Owner.Granted
			id = google.Identity{Sub: cur.Owner.Sub, Email: cur.Owner.Email}
			o.say("  the owner's stored grant (%s) works; --reconsent asks again", cur.Owner.Email)
		case code == google.CodeInvalidGrant || code == google.CodeScope:
			o.say("  the owner's stored grant (%s) no longer works; asking again", cur.Owner.Email)
		default:
			return px.explain(err, google.Owner)
		}
	}
	if access == "" {
		hint := ""
		if have && !p.replace {
			hint = cur.Owner.Email
			o.say("  Sign in as the push owner, %s, and allow Pub/Sub.", hint)
		} else {
			o.say("  Sign in as the identity that owns project %s (the push owner) and allow Pub/Sub.", c.Project)
		}
		tok, err := e.consent(c.Credentials(), google.Owner, hint, px)
		if err != nil {
			return err
		}
		if tok.Identity == nil {
			return errors.New("Google's answer named no identity; nothing changed")
		}
		if have && !p.replace && tok.Identity.Sub != cur.Owner.Sub {
			return fmt.Errorf("consent was given as a different Google account than the push owner, %s, so nothing changed: sign in as %s, or set up a new owner with --replace (every account's push off first)", cur.Owner.Email, cur.Owner.Email)
		}
		access, refresh, granted, id = tok.Access, tok.Refresh, e.now(), *tok.Identity
	}

	// The probe: the owner's token reaches the project's Pub/Sub.
	if err := e.api.ProbeTopics(ctx, access, c.Project); err != nil {
		return px.explain(err, google.Owner)
	}
	o.say("  %s reaches project %s's Pub/Sub", id.Email, c.Project)

	// The commit: re-read under the lock, the same checks, then the
	// client and the state.
	l, err := e.lock()
	if err != nil {
		return err
	}
	defer func() {
		if l != nil {
			l.Unlock()
		}
	}()
	now, err := l.Current()
	haveNow := err == nil
	if err != nil && !errors.Is(err, state.ErrNoState) {
		return err
	}
	if err := checkInit(now.File, haveNow, c, p.replace); err != nil {
		return err
	}
	if haveNow != have || (haveNow && (now.Owner.Sub != cur.Owner.Sub || now.Client != cur.Client)) {
		return errors.New("push's setup changed while this ran (another pneu push init?); nothing changed: run this again")
	}
	if _, err := l.WriteClient(cleanPushClient(c)); err != nil {
		return err
	}
	snap, err := l.Update(func(f *state.File) error {
		f.Project, f.Client = c.Project, c.ID
		if f.Install == "" {
			f.Install = state.NewInstall()
		}
		f.Owner = state.Owner{Sub: id.Sub, Email: id.Email, Refresh: refresh, Granted: granted}
		return nil
	})
	if err != nil {
		return err
	}
	o.say("  committed: owner %s, project %s (generation %d)", id.Email, c.Project, snap.Generation)
	h, dl, _, why := e.handOver(l, snap)
	l.Unlock()
	l = nil
	if dl != nil {
		dl.Unlock()
	}
	switch h {
	case handNoDaemon:
		o.say("pneu isn't running; it takes this when it starts.")
	case handNoPush:
		o.say("The running pneu runs no push sync; restart it to start: systemctl --user restart pneu")
	case handPending:
		return fmt.Errorf("push is set up, but the running pneu hasn't confirmed it (%s): pending. Running pneu push init again is safe", why)
	default:
		o.say("The running pneu has it.")
	}
	o.say("Push is set up. Next, for each account: pneu account push <name>")
	return nil
}

// checkInit is init's rule against the state as read: a different client
// or project needs --replace, and --replace needs every account off (each
// one's credential is the old client's, and its topic in the old
// project's).
func checkInit(f state.File, have bool, c state.Client, replace bool) error {
	if !have {
		return nil
	}
	if (c.ID != f.Client || c.Project != f.Project) && !replace {
		return fmt.Errorf("push is set up with client %s in project %s; a different client or project needs --replace", f.Client, f.Project)
	}
	if replace && len(f.Accounts) > 0 {
		names := slices.Sorted(maps.Keys(f.Accounts))
		return fmt.Errorf("--replace needs every account's push off first: pneu account push <name> --off for %s", strings.Join(names, ", "))
	}
	return nil
}

// --- pneu account push <name> ----------------------------------------

// loadSetup is push's setup as the commands need it: the state (read
// without the lock) and the client, checked against each other.
func (e *pushEnv) loadSetup() (state.Snapshot, state.Client, error) {
	snap, err := e.store.Load()
	if errors.Is(err, state.ErrNoState) {
		return state.Snapshot{}, state.Client{}, errors.New("push isn't set up here: run pneu push init --project <id> --client-secret <file> first")
	}
	if err != nil {
		return state.Snapshot{}, state.Client{}, err
	}
	c, err := e.store.LoadClient()
	if err != nil {
		return state.Snapshot{}, state.Client{}, fmt.Errorf("the stored push client: %w", err)
	}
	if err := state.CheckClient(snap.File, c); err != nil {
		return state.Snapshot{}, state.Client{}, err
	}
	return snap, c, nil
}

// holder is the account other than name that holds address, or "".
func holder(f state.File, name, address string) string {
	for n, a := range f.Accounts {
		if n != name && google.SameAddress(a.Address, address) {
			return n
		}
	}
	return ""
}

// errSetupChanged: push init ran between the read and the commit.
var errSetupChanged = errors.New("push's setup changed while this ran (pneu push init?); nothing was committed: run this again")

// sameSetup reports whether f is still the setup read as was.
func sameSetup(f, was state.File) error {
	if f.Generation == 0 || f.Project != was.Project || f.Client != was.Client || f.Install != was.Install || f.Owner.Sub != was.Owner.Sub {
		return errSetupChanged
	}
	return nil
}

// on turns instant mail on for an account (D4 steps 1–5).
func (e *pushEnv) on(name string, reconsent bool) error {
	o := e.out
	ctx := o.ctx()
	a, err := loadAccount(e.cfgPath, name)
	if err != nil {
		return err
	}
	address := a.Email
	if !google.ValidAddress(address) {
		return fmt.Errorf("%s's address isn't one push can hold (a plain ASCII address)", name)
	}
	res, ok := google.Resource(name)
	if !ok {
		return fmt.Errorf("%q can't name a Pub/Sub topic", name)
	}
	snap, c, err := e.loadSetup()
	if err != nil {
		return err
	}
	px := pushCtx{project: snap.Project, name: name, address: address, owner: snap.Owner.Email}
	if other := holder(snap.File, name, address); other != "" {
		return fmt.Errorf("%s is already pushed as account %s: one mailbox, one topic", address, other)
	}
	o.say("Instant mail for %s (%s), push project %s", name, address, snap.Project)
	owner, err := e.api.Refresh(ctx, c.Credentials(), google.Owner, snap.Owner.Refresh, snap.Owner.Sub)
	if err != nil {
		return px.explain(err, google.Owner)
	}

	// 1. The mailbox's grant: the stored one when it's this address's and
	// still works, else a consent; getProfile must name the address.
	var (
		access, refresh string
		granted         time.Time
	)
	if prev, had := snap.Accounts[name]; had && google.SameAddress(prev.Address, address) && !reconsent {
		tok, err := e.api.Refresh(ctx, c.Credentials(), google.Mailbox, prev.Refresh, "")
		switch code := google.CodeOf(err); {
		case err == nil:
			access, refresh, granted = tok.Access, prev.Refresh, prev.Granted
			o.say("  the stored mailbox grant works; --reconsent asks again")
		case code == google.CodeInvalidGrant || code == google.CodeScope:
			o.say("  the stored mailbox grant no longer works; asking again")
		default:
			return px.explain(err, google.Mailbox)
		}
	}
	if access == "" {
		o.say("  Sign in as %s and allow pneu to see its mail's metadata (labels and headers, never bodies).", address)
		tok, err := e.consent(c.Credentials(), google.Mailbox, address, px)
		if err != nil {
			return err
		}
		access, refresh, granted = tok.Access, tok.Refresh, e.now()
	}
	got, err := e.api.Profile(ctx, access)
	if err != nil {
		return px.explain(err, google.Mailbox)
	}
	if !google.SameAddress(got, address) {
		return fmt.Errorf("the mailbox chosen at Google's consent isn't %s, so nothing was set up: run this again and sign in as %s", address, address)
	}
	o.say("  Gmail answers for %s", address)

	// 2. Provisioning, with the owner's token; each step idempotent.
	created, err := e.api.EnsureTopic(ctx, owner.Access, snap.Project, res)
	if err != nil {
		return px.explain(err, google.Owner)
	}
	o.say("  topic %s: %s", res, pick(created, "created", "already there"))
	changed, err := e.api.GrantPublisher(ctx, owner.Access, snap.Project, res)
	if err != nil {
		return px.explain(err, google.Owner)
	}
	o.say("  Gmail may publish to it: %s", pick(changed, "granted", "already granted"))
	subCreated, err := e.api.EnsureSubscription(ctx, owner.Access, snap.Project, res, res, snap.Install)
	if err != nil {
		return px.explain(err, google.Owner)
	}
	o.say("  subscription %s: %s", res, pick(subCreated, "created", "already there, as pneu makes it"))

	// 3. The commit, re-read under the lock; 4. the hand-over.
	l, err := e.lock()
	if err != nil {
		return err
	}
	next, err := l.Update(func(f *state.File) error {
		if err := sameSetup(*f, snap.File); err != nil {
			return err
		}
		if other := holder(*f, name, address); other != "" {
			return fmt.Errorf("%s was pushed as account %s while this ran; nothing was committed", address, other)
		}
		f.Accounts[name] = state.Account{State: state.On, Address: address, Refresh: refresh, Granted: granted}
		return nil
	})
	if err != nil {
		l.Unlock()
		return err
	}
	o.say("  committed: %s is on (generation %d)", name, next.Generation)
	h, dl, next, why := e.handOver(l, next)
	l.Unlock()
	if dl != nil {
		dl.Unlock()
	}
	switch h {
	case handNoDaemon:
		o.say("pneu isn't running; instant mail for %s starts when it does.", name)
		return nil
	case handNoPush:
		o.say("The running pneu runs no push sync; restart it to start instant mail: systemctl --user restart pneu")
		return nil
	case handPending:
		return fmt.Errorf("%s is on in state.json, but the running pneu hasn't confirmed it (%s): pending. Running pneu account push %s again is safe", name, why, name)
	}
	// 5. The proof.
	return e.awaitDelivery(name, next.Generation, !subCreated)
}

func pick(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// awaitDelivery waits up to deliverWait for the daemon to report the
// account delivering at generation gen: it renewed the watch and then
// received a message (a watch publishes one at once). A timeout leaves
// everything in place and says what's unproven.
func (e *pushEnv) awaitDelivery(name string, gen uint64, reused bool) error {
	o := e.out
	o.say("  waiting up to %v for Gmail's first notification", e.deliverWait)
	deadline := time.Now().Add(e.deliverWait)
	instance := ""
	var last control.PushState
	for {
		p, err := control.AskPushState(e.socket, name)
		switch {
		case errors.Is(err, control.ErrNotRunning) || errors.Is(err, control.ErrPushOff):
			o.say("pneu stopped answering while this waited; %s is on, and instant mail starts when pneu runs push again.", name)
			return nil
		case err != nil:
			// A question that failed (busy, a slow answer) proves nothing
			// either way: keep asking until the deadline.
		case p.Generation > gen:
			o.say("A later push generation (%d) is live already, so this run can't prove its own; run pneu account push %s again to check.", p.Generation, name)
			return nil
		case p.Generation == gen:
			if instance != "" && p.Instance != instance {
				o.say("  (pneu restarted meanwhile)")
			}
			instance, last = p.Instance, p
			switch p.State {
			case control.PushDelivering:
				if reused {
					o.say("Instant mail is on for %s: its subscription is delivering. It existed before this run, so that shows messages arrive, not that this watch sent them.", name)
				} else {
					o.say("Instant mail is on for %s: Gmail's watch delivered its first notification.", name)
				}
				return nil
			case control.PushReauth:
				return fmt.Errorf("%s is on, but pneu reports %s", name, reasonWords(p.Reason, name))
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-o.ctx().Done():
			return errors.New("stopped while waiting for the first notification; everything stays in place")
		case <-time.After(e.pollEvery):
		}
	}
	seen := "no answer about it"
	if last.State != "" {
		seen = "it reports " + last.State
		if last.Reason != "" {
			seen += ": " + reasonWords(last.Reason, name)
		}
	}
	o.say("Instant mail is set up for %s, but not proven: no notification reached pneu within %v (%s). Everything stays in place, and running pneu account push %s again is safe.", name, e.deliverWait, seen, name)
	return nil
}

// --- pneu account push <name> --off ----------------------------------

// off turns instant mail off for an account (D4, K2): off-pending
// committed, the daemon's worker confirmed gone, users.stop with the
// mailbox's token, then the account's removal. Topic and subscription
// stay; pneu never revokes a grant.
func (e *pushEnv) off(name string) error {
	o := e.out
	l, err := e.lock()
	if err != nil {
		return err
	}
	snap, err := l.Current()
	if errors.Is(err, state.ErrNoState) {
		l.Unlock()
		return errors.New("push isn't set up here; there's nothing to turn off")
	}
	if err != nil {
		l.Unlock()
		return err
	}
	acct, ok := snap.Accounts[name]
	if !ok {
		l.Unlock()
		o.say("Instant mail is already off for %s.", name)
		return nil
	}
	// 1. off-pending, the token kept for users.stop.
	if acct.State == state.On {
		snap, err = l.Update(func(f *state.File) error {
			a, ok := f.Accounts[name]
			if !ok {
				return errors.New("unreachable: the account went under the lock")
			}
			a.State = state.OffPending
			f.Accounts[name] = a
			return nil
		})
		if err != nil {
			l.Unlock()
			return err
		}
		o.say("  committed: %s is off-pending (generation %d)", name, snap.Generation)
	} else {
		o.say("  %s is off-pending already: retrying Gmail's cleanup", name)
	}
	// 2. The worker gone: acknowledged, or no daemon (daemon.lock held
	// until the removal is committed).
	h, dl, _, why := e.handOver(l, snap)
	l.Unlock()
	if h == handPending {
		return fmt.Errorf("%s is off in state.json, but the running pneu hasn't confirmed its worker is gone (%s): pending; the daemon applies it when it answers. Nothing was deleted; run pneu account push %s --off again", name, why, name)
	}
	if dl != nil {
		defer dl.Unlock()
	}

	// 3. users.stop with the mailbox's token.
	px := pushCtx{project: snap.Project, name: name, address: acct.Address, owner: snap.Owner.Email}
	lapse, stopErr := e.stopWatch(snap.File, acct, px)
	if stopErr != nil {
		return fmt.Errorf("%s is off, but Gmail's watch wasn't stopped (%v): remote cleanup is pending. Run pneu account push %s --off again to retry; the watch also lapses by itself within 7 days", name, stopErr, name)
	}

	// 4. The account's removal.
	l, err = e.lock()
	if err != nil {
		return err
	}
	removed, err := l.Update(func(f *state.File) error {
		a, ok := f.Accounts[name]
		switch {
		case !ok:
			return errGone
		case a.State != state.OffPending || a.Refresh != acct.Refresh:
			return errReenabled
		}
		delete(f.Accounts, name)
		return nil
	})
	switch {
	case errors.Is(err, errGone):
		l.Unlock()
	case errors.Is(err, errReenabled):
		l.Unlock()
		return fmt.Errorf("instant mail was turned on again for %s while this ran; left it on", name)
	case err != nil:
		l.Unlock()
		return err
	default:
		if dl == nil {
			// The daemon has no worker for it either way; this only moves
			// its generation on.
			if h2, _, _, _ := e.handOver(l, removed); h2 == handPending {
				o.say("  (the running pneu hasn't confirmed generation %d; it changes nothing it runs)", removed.Generation)
			}
		}
		l.Unlock()
		o.say("  committed: %s removed (generation %d)", name, removed.Generation)
	}
	if lapse {
		o.say("%s's grant no longer works, so pneu couldn't stop its watch; it lapses by itself within 7 days.", acct.Address)
	}
	res, _ := google.Resource(name)
	o.say("Instant mail is off for %s.", name)
	o.say("Its topic and subscription stay in project %s. To delete them:", snap.Project)
	o.say("  gcloud pubsub subscriptions delete %s --project=%s", res, snap.Project)
	o.say("  gcloud pubsub topics delete %s --project=%s", res, snap.Project)
	o.say("  (or https://console.cloud.google.com/cloudpubsub/topic/list?project=%s)", snap.Project)
	o.say("pneu never revokes a grant. To revoke the push app's access to %s, sign in as %s at https://myaccount.google.com/permissions.", acct.Address, acct.Address)
	if google.SameAddress(acct.Address, snap.Owner.Email) {
		o.say("%s is also the push owner: revoking either grant there ends both, and with it instant mail for every account.", acct.Address)
	}
	return nil
}

var (
	errGone      = errors.New("the account is gone already")
	errReenabled = errors.New("the account was turned on again")
)

// stopWatch runs users.stop with the mailbox's token. A refresh refused
// with invalid_grant can never stop it (lapse: the watch runs out within
// 7 days), and counts as done; any other failure is returned in words.
func (e *pushEnv) stopWatch(f state.File, a state.Account, px pushCtx) (lapse bool, err error) {
	c, err := e.store.LoadClient()
	if err != nil {
		return false, fmt.Errorf("the stored push client: %w", err)
	}
	if err := state.CheckClient(f, c); err != nil {
		return false, err
	}
	ctx := e.out.ctx()
	tok, err := e.api.Refresh(ctx, c.Credentials(), google.Mailbox, a.Refresh, "")
	if google.CodeOf(err) == google.CodeInvalidGrant {
		return true, nil
	}
	if err != nil {
		return false, px.explain(err, google.Mailbox)
	}
	if err := e.api.Stop(ctx, tok.Access); err != nil {
		return false, px.explain(err, google.Mailbox)
	}
	e.out.say("  Gmail's watch on %s is stopped", a.Address)
	return false, nil
}

// --- the hand-over ----------------------------------------------------

// handover is how a push-reload came out.
type handover int

const (
	// handApplied: "ok <gen>": the daemon applies this generation.
	handApplied handover = iota + 1
	// handNoDaemon: nothing answered and daemon.lock was free: no daemon
	// runs push. The lock is returned held.
	handNoDaemon
	// handNoPush: a pneu answered but runs no push sync, and daemon.lock
	// was free. The lock is returned held.
	handNoPush
	// handPending: sent, not confirmed.
	handPending
)

// handOver sends snap to the daemon with push-reload, the caller holding
// push.lock, and says what came of it, never more than the answer says.
// A mismatch is re-read and sent again; "no daemon" is decided only by
// taking daemon.lock. It returns the snapshot it last sent, and for
// handPending why, in pneu's words.
func (e *pushEnv) handOver(l *state.Locked, snap state.Snapshot) (handover, *state.DaemonLock, state.Snapshot, string) {
	for range reloadTries {
		var r control.Reload
		err := e.sockErr
		if err == nil {
			r, err = control.ReloadPush(e.socket, snap.Generation, snap.Hash)
		}
		switch {
		case err == nil && r == control.ReloadApplied:
			return handApplied, nil, snap, ""
		case err == nil && r == control.ReloadMismatch:
			cur, err := l.Current()
			if err != nil {
				return handPending, nil, snap, "state.json couldn't be read again: " + err.Error()
			}
			snap = cur
			continue
		case err == nil && r == control.ReloadStale:
			return handPending, nil, snap, fmt.Sprintf("it has applied a newer generation than state.json's %d; restart it: systemctl --user restart pneu", snap.Generation)
		case e.sockErr != nil || errors.Is(err, control.ErrNotRunning) || errors.Is(err, control.ErrPushOff):
			d, derr := e.store.TryDaemonLock()
			switch {
			case derr == nil && errors.Is(err, control.ErrPushOff):
				return handNoPush, d, snap, ""
			case derr == nil:
				return handNoDaemon, d, snap, ""
			case errors.Is(derr, state.ErrDaemonRunning):
				return handPending, nil, snap, "its push sync holds its lock, but its control socket didn't take the reload"
			default:
				return handPending, nil, snap, derr.Error()
			}
		default:
			return handPending, nil, snap, "no acknowledgment from it"
		}
	}
	return handPending, nil, snap, "it kept finding a different state.json"
}
