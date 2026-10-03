package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/googletest"
	"github.com/jmckible/pneu/internal/push/state"
)

const (
	ownerAddr = "owner@gmail.com"
	mailAddr  = "me@example.com"
)

// pushTest is a pushEnv against googletest's fake: state in a temp dir,
// a config with accounts, a control socket path nothing listens on until
// a stub daemon starts, and a terminal whose consent binds a free port on
// both loopbacks (never 8080) and whose "browser" answers the consent.
type pushTest struct {
	t    *testing.T
	f    *googletest.Fake
	e    *pushEnv
	out  *lockedBuf
	cfg  string
	port int
	// as is who signs in at the consent screen; browse, when set, plays
	// the browser instead (given the consent URL and the port).
	mu     sync.Mutex
	as     string
	browse func(u string, port int)
	opened int
	errs   chan error
}

// lockedBuf is a terminal both the command and the browser write to.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuf) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// pushClientJSON is a Desktop client JSON for the fake's client.
func pushClientJSON(f *googletest.Fake) []byte {
	return []byte(fmt.Sprintf(`{"installed":{"client_id":%q,"project_id":%q,"auth_uri":"https://accounts.google.com/o/oauth2/auth","client_secret":%q,"redirect_uris":["http://localhost"]}}`,
		f.ClientID, f.Project, f.Secret))
}

func newPushTest(t *testing.T, accounts ...string) *pushTest {
	t.Helper()
	f := googletest.New(t)
	f.AddUser(ownerAddr)
	f.AddUser(mailAddr)
	dir := t.TempDir()
	if len(accounts) == 0 {
		accounts = []string{"personal", mailAddr}
	}
	var accts []string
	for i := 0; i+1 < len(accounts); i += 2 {
		accts = append(accts, fmt.Sprintf(`{"name":%q,"email":%q,"notmuchConfig":"/nonexistent/nm","gmiDir":"/nonexistent/gmi"}`, accounts[i], accounts[i+1]))
	}
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"port":7317,"accounts":[`+strings.Join(accts, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sock, err := os.MkdirTemp("", "pneupush")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	pt := &pushTest{t: t, f: f, out: &lockedBuf{}, cfg: cfg, as: mailAddr, errs: make(chan error, 16)}
	pt.e = &pushEnv{cfgPath: cfg, store: state.Store{Dir: state.Dir(filepath.Join(dir, "state", "pneu"))}, api: f.API(),
		socket: filepath.Join(sock, "pneu", "control"), now: f.Clock.Now, deliverWait: 2 * time.Second, pollEvery: 20 * time.Millisecond}
	pt.e.out = termPush{w: pt.out,
		listen: func() ([]net.Listener, error) {
			pt.port = freeBothPort(t)
			return listenLoopback(pt.port, bindExclusive)
		},
		open: func(u string) error {
			pt.mu.Lock()
			pt.opened++
			browse, as, port := pt.browse, pt.as, pt.port
			pt.mu.Unlock()
			go func() {
				if browse != nil {
					browse(u, port)
					return
				}
				q, err := f.Approve(u, as)
				if err != nil {
					pt.errs <- err
					return
				}
				if code, _ := redirect(port, q); code != http.StatusOK {
					pt.errs <- fmt.Errorf("the redirect got %d", code)
				}
			}()
			return nil
		}}
	return pt
}

// redirect plays Google sending the browser to the callback port.
func redirect(port int, query string) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/?"+query, nil)
	req.Host = "localhost:" + strconv.Itoa(port)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (pt *pushTest) signInAs(email string) {
	pt.mu.Lock()
	pt.as = email
	pt.mu.Unlock()
}

func (pt *pushTest) consents() int {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.opened
}

func (pt *pushTest) init(p initParams) error {
	pt.t.Helper()
	pt.signInAs(ownerAddr)
	defer pt.signInAs(mailAddr)
	return pt.e.init(p)
}

func (pt *pushTest) firstInit() {
	pt.t.Helper()
	if err := pt.init(initParams{project: pt.f.Project, client: pushClientJSON(pt.f)}); err != nil {
		pt.t.Fatalf("init: %v\n%s", err, pt.out.String())
	}
}

func (pt *pushTest) load() state.Snapshot {
	pt.t.Helper()
	s, err := pt.e.store.Load()
	if err != nil {
		pt.t.Fatal(err)
	}
	return s
}

// noGoogleText fails if anything Google wrote reached the terminal.
func (pt *pushTest) noGoogleText() {
	pt.t.Helper()
	for _, s := range []string{"Google's words", "never shown", "permitted customer", "Bad Request", "ya29.", "1//", "GOCSPX"} {
		if strings.Contains(pt.out.String(), s) {
			pt.t.Fatalf("output carries %q:\n%s", s, pt.out.String())
		}
	}
}

// stubDaemon plays the daemon's push side on the control socket: it holds
// daemon.lock, applies a generation only when state.json is that
// generation with that hash, and answers push-state for applied accounts
// with state (default delivering).
type stubDaemon struct {
	t       *testing.T
	store   state.Store
	mu      sync.Mutex
	applied uint64
	reloads []uint64
	// reload, when set, answers instead.
	reload func(gen uint64, hash string) (control.Reload, error)
	state  func(account string, gen uint64) control.PushState
	srv    *control.Server
	dl     *state.DaemonLock
	once   sync.Once
}

// stop plays the daemon exiting: its socket closed, daemon.lock released.
func (d *stubDaemon) stop() {
	d.once.Do(func() {
		d.srv.Close()
		d.dl.Unlock()
	})
}

// stubInstance is what the stub answers status with: this process's.
var stubInstance = control.Instance()

func startStubDaemon(t *testing.T, e *pushEnv) *stubDaemon {
	t.Helper()
	d := &stubDaemon{t: t, store: e.store}
	dl, err := e.store.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	d.dl = dl
	s, err := control.Listen(e.socket, control.Handler{
		PushReload: func(gen uint64, hash string) (control.Reload, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.reloads = append(d.reloads, gen)
			if d.reload != nil {
				return d.reload(gen, hash)
			}
			return d.apply(gen, hash)
		},
		PushState: func(account string) control.PushState {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.state != nil {
				return d.state(account, d.applied)
			}
			snap, err := d.store.Load()
			if err != nil || snap.Generation != d.applied || snap.Accounts[account].State != state.On {
				return control.PushState{Instance: stubInstance, Generation: d.applied, State: control.PushOff}
			}
			return control.PushState{Instance: stubInstance, Generation: d.applied, State: control.PushDelivering, LastDelivery: e.now()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.srv = s
	t.Cleanup(d.stop)
	return d
}

// apply is the daemon's rule; d.mu held.
func (d *stubDaemon) apply(gen uint64, hash string) (control.Reload, error) {
	if gen < d.applied {
		return control.ReloadStale, nil
	}
	snap, err := d.store.Load()
	if err != nil || snap.Generation != gen || snap.Hash != hash {
		return control.ReloadMismatch, nil
	}
	d.applied = gen
	return control.ReloadApplied, nil
}

func (d *stubDaemon) reloadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reloads)
}

// Init, then turning an account on: the owner's consent (with a nonce,
// which the fake insists on), sub pinned, the probe, the first commit;
// the mailbox's consent, getProfile, the topic, the publisher grant, the
// labelled subscription, the commit, the daemon's ack, and delivering.
func TestPushInitAndOn(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	snap := pt.load()
	if snap.Generation != 1 || snap.Project != pt.f.Project || snap.Client != pt.f.ClientID || snap.Owner.Email != ownerAddr ||
		!google.ValidInstall(snap.Install) || snap.Owner.Sub == "" || len(snap.Accounts) != 0 {
		t.Fatalf("state after init: %+v", snap.File)
	}
	c, err := pt.e.store.LoadClient()
	if err != nil || c.ID != pt.f.ClientID || c.Secret != pt.f.Secret {
		t.Fatalf("client.json: %v %v", c, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(pt.e.store.Dir, state.ClientFile)); strings.Contains(string(raw), "redirect_uris") {
		t.Errorf("client.json keeps what pneu doesn't use: %s", raw)
	}
	if pt.f.Calls(google.OpTopicList) != 1 || d.reloadCount() != 1 || pt.consents() != 1 {
		t.Fatalf("probes %d, reloads %d, consents %d", pt.f.Calls(google.OpTopicList), d.reloadCount(), pt.consents())
	}
	if !strings.Contains(pt.out.String(), "The running pneu has it.") {
		t.Errorf("init output:\n%s", pt.out.String())
	}

	pt.out.Reset()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatalf("on: %v\n%s", err, pt.out.String())
	}
	snap = pt.load()
	a := snap.Accounts["personal"]
	if snap.Generation != 2 || a.State != state.On || a.Address != mailAddr || !google.ValidRefresh(a.Refresh) {
		t.Fatalf("state after on: %+v", snap.File)
	}
	if !pt.f.HasTopic("pneu-personal") || !strings.Contains(pt.f.Policy("pneu-personal"), google.PublisherMember) ||
		!strings.Contains(pt.f.Subscription("pneu-personal"), snap.Install) {
		t.Fatalf("provisioning: topic %v, policy %s, sub %s", pt.f.HasTopic("pneu-personal"), pt.f.Policy("pneu-personal"), pt.f.Subscription("pneu-personal"))
	}
	if !strings.Contains(pt.out.String(), "Instant mail is on for personal: Gmail's watch delivered its first notification.") {
		t.Errorf("on output:\n%s", pt.out.String())
	}
	pt.noGoogleText()

	// Again: the stored grant is reused (no consent), everything found in
	// place, and delivering is worded as not proving this watch.
	pt.out.Reset()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatalf("again: %v\n%s", err, pt.out.String())
	}
	if pt.consents() != 2 || pt.load().Accounts["personal"].Refresh != a.Refresh {
		t.Fatalf("consents %d; refresh kept %v", pt.consents(), pt.load().Accounts["personal"].Refresh == a.Refresh)
	}
	for _, want := range []string{"stored mailbox grant works", "already there", "already granted", "not that this watch sent them"} {
		if !strings.Contains(pt.out.String(), want) {
			t.Errorf("again: no %q in\n%s", want, pt.out.String())
		}
	}
	if pt.f.PolicyWrites("pneu-personal") != 1 {
		t.Errorf("policy written %d times", pt.f.PolicyWrites("pneu-personal"))
	}
	// --reconsent asks anyway.
	if err := pt.e.on("personal", true); err != nil || pt.consents() != 3 {
		t.Fatalf("reconsent: %v, consents %d", err, pt.consents())
	}
	select {
	case err := <-pt.errs:
		t.Fatal(err)
	default:
	}
}

// Init's rules: the first needs both flags; the client must be the
// project's; a different client needs --replace, and --replace needs
// every account off.
func TestPushInitRefusals(t *testing.T) {
	pt := newPushTest(t)
	for _, p := range []initParams{
		{project: pt.f.Project},
		{client: pushClientJSON(pt.f)},
		{project: "other-project", client: pushClientJSON(pt.f)},
		{project: pt.f.Project, client: []byte(`{"web":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s","project_id":"pneu-push-test"}}`)},
	} {
		if err := pt.init(p); err == nil {
			t.Errorf("%+v: accepted", p)
		}
	}
	if pt.consents() != 0 {
		t.Fatalf("a refused init consented")
	}
	if _, err := pt.e.store.Load(); !errors.Is(err, state.ErrNoState) {
		t.Fatalf("state after refusals: %v", err)
	}
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	other := []byte(strings.Replace(string(pushClientJSON(pt.f)), pt.f.ClientID, "999-other.apps.googleusercontent.com", 1))
	before := pt.consents()
	if err := pt.init(initParams{client: other}); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("a different client: %v", err)
	}
	if err := pt.init(initParams{client: other, replace: true}); err == nil || !strings.Contains(err.Error(), "pneu account push <name> --off for personal") {
		t.Fatalf("--replace with an account on: %v", err)
	}
	if pt.consents() != before || pt.load().Generation != 2 {
		t.Fatal("a refused re-init consented or committed")
	}
}

// A push client that is an account's lieer client is refused before any
// consent (D1).
func TestPushInitRefusesLieerClient(t *testing.T) {
	pt := newPushTest(t)
	nm := filepath.Join(t.TempDir(), "notmuch-config")
	cfg := `{"port":7317,"accounts":[{"name":"personal","email":"` + mailAddr + `","notmuchConfig":"` + nm + `","gmiDir":"/nonexistent/gmi"}]}`
	if err := os.WriteFile(pt.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	lieer := `{"installed":{"client_id":"` + pt.f.ClientID + `","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(nm), "client_secret.json"), []byte(lieer), 0o600); err != nil {
		t.Fatal(err)
	}
	err := pt.init(initParams{project: pt.f.Project, client: pushClientJSON(pt.f)})
	if err == nil || !strings.Contains(err.Error(), "account personal's lieer client") {
		t.Fatalf("lieer's client as push's: %v", err)
	}
	if pt.consents() != 0 {
		t.Fatal("a refused init consented")
	}
}

// Re-init: the stored owner grant is reused; --reconsent must come back
// as the pinned owner; a revoked owner grant consents by itself; a
// rotated secret for the same client is taken.
func TestPushInitAgain(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	sub := pt.load().Owner.Sub
	if err := pt.init(initParams{}); err != nil || pt.consents() != 1 || pt.load().Generation != 2 {
		t.Fatalf("reuse: %v, consents %d, gen %d", err, pt.consents(), pt.load().Generation)
	}
	if !strings.Contains(pt.out.String(), "stored grant (owner@gmail.com) works") {
		t.Errorf("output:\n%s", pt.out.String())
	}
	// --reconsent as someone else: refused, nothing changed.
	pt.f.AddUser("intruder@gmail.com")
	pt.mu.Lock()
	pt.browse = func(u string, port int) {
		q, _ := pt.f.Approve(u, "intruder@gmail.com")
		redirect(port, q)
	}
	pt.mu.Unlock()
	err := pt.e.init(initParams{reconsent: true})
	if err == nil || !strings.Contains(err.Error(), "different Google account than the push owner, owner@gmail.com") {
		t.Fatalf("another owner: %v", err)
	}
	if s := pt.load(); s.Generation != 2 || s.Owner.Sub != sub {
		t.Fatalf("state changed: %+v", s.File)
	}
	pt.mu.Lock()
	pt.browse = nil
	pt.mu.Unlock()
	// The owner revokes: the next init asks again, as them.
	pt.f.Revoke(ownerAddr)
	if err := pt.init(initParams{}); err != nil || pt.consents() != 3 {
		t.Fatalf("after revoke: %v, consents %d", err, pt.consents())
	}
	if s := pt.load(); s.Owner.Sub != sub || s.Generation != 3 {
		t.Fatalf("state: %+v", s.File)
	}
	// --replace (no accounts): a new owner is fine.
	pt.mu.Lock()
	pt.browse = func(u string, port int) {
		q, _ := pt.f.Approve(u, "intruder@gmail.com")
		redirect(port, q)
	}
	pt.mu.Unlock()
	if err := pt.e.init(initParams{replace: true}); err != nil {
		t.Fatal(err)
	}
	if s := pt.load(); s.Owner.Email != "intruder@gmail.com" || s.Owner.Sub == sub {
		t.Fatalf("replace: %+v", s.File)
	}
}

// The probe failing commits nothing, and says what to do in pneu's words.
func TestPushInitProbeFails(t *testing.T) {
	for _, tc := range []struct {
		code google.Code
		want string
	}{
		{google.CodeAPIDisabled, "the Cloud Pub/Sub API isn't enabled in project pneu-push-test. Enable it at https://console.cloud.google.com/apis/library/pubsub.googleapis.com?project=pneu-push-test"},
		{google.CodePermission, "lacks permission in project pneu-push-test"},
		{google.CodeNotFound, "project pneu-push-test doesn't exist"},
	} {
		pt := newPushTest(t)
		pt.f.FailCode(google.OpTopicList, tc.code, 1)
		err := pt.init(initParams{project: pt.f.Project, client: pushClientJSON(pt.f)})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.code, err)
		}
		if _, err := pt.e.store.Load(); !errors.Is(err, state.ErrNoState) {
			t.Errorf("%s: state written: %v", tc.code, err)
		}
		pt.noGoogleText()
	}
}

// Setup interrupted at each step changes nothing it shouldn't, and a
// rerun finishes it: one topic, one grant, one subscription.
func TestPushOnInterrupted(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	for _, op := range []google.Op{google.OpRefresh, google.OpExchange, google.OpProfile, google.OpTopicMake,
		google.OpGetPolicy, google.OpSetPolicy, google.OpSubMake} {
		pt.f.FailCode(op, google.CodeUnavailable, 1)
		err := pt.e.on("personal", false)
		if err == nil || !strings.Contains(err.Error(), "Google's side failed") {
			t.Fatalf("%s: %v", op, err)
		}
		if _, ok := pt.load().Accounts["personal"]; ok {
			t.Fatalf("%s: committed after a failure", op)
		}
	}
	// The ack lost: committed, said pending; a rerun confirms.
	d.mu.Lock()
	d.reload = func(uint64, string) (control.Reload, error) { return 0, errors.New("busy") }
	d.mu.Unlock()
	err := pt.e.on("personal", false)
	if err == nil || !strings.Contains(err.Error(), "hasn't confirmed it (no acknowledgment from it): pending") {
		t.Fatalf("lost ack: %v", err)
	}
	if pt.load().Accounts["personal"].State != state.On {
		t.Fatal("not committed before the reload")
	}
	d.mu.Lock()
	d.reload = nil
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatalf("rerun: %v\n%s", err, pt.out.String())
	}
	if pt.f.PolicyWrites("pneu-personal") != 1 || pt.f.Calls(google.OpSubMake) < 1 {
		t.Errorf("policy writes %d", pt.f.PolicyWrites("pneu-personal"))
	}
	pt.noGoogleText()
}

// Each provisioning failure is told by its code, with the fix.
func TestPushOnProvisionWords(t *testing.T) {
	for _, tc := range []struct {
		op   google.Op
		code google.Code
		want string
	}{
		{google.OpTopicMake, google.CodeAPIDisabled, "creating the topic failed: the Cloud Pub/Sub API isn't enabled in project pneu-push-test"},
		{google.OpProfile, google.CodeAPIDisabled, "the Gmail API isn't enabled in project pneu-push-test. Enable it at https://console.cloud.google.com/apis/library/gmail.googleapis.com?project=pneu-push-test"},
		{google.OpSetPolicy, google.CodeOrgPolicy, "Override iam.allowedPolicyMemberDomains to Allow All on project pneu-push-test only"},
		{google.OpExchange, google.CodeOrgPolicy, "me@example.com's Workspace admin doesn't allow this app"},
		{google.OpTopicMake, google.CodePermission, "the push owner (owner@gmail.com) lacks permission in project pneu-push-test"},
		{google.OpSubMake, google.CodeQuota, "Google's rate limit"},
		{google.OpTopicMake, google.CodeNetwork, "couldn't reach Google"},
	} {
		pt := newPushTest(t)
		pt.firstInit()
		pt.f.FailCode(tc.op, tc.code, 1)
		err := pt.e.on("personal", false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v", tc.op, tc.code, err)
		}
		pt.noGoogleText()
	}
	// The owner's grant gone: the fix is init's.
	pt := newPushTest(t)
	pt.firstInit()
	pt.f.Revoke(ownerAddr)
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "the push owner's grant (owner@gmail.com) no longer works (revoked, or expired): run pneu push init --reconsent") {
		t.Errorf("owner revoked: %v", err)
	}
	if pt.consents() != 1 {
		t.Error("consented for a mailbox with the owner's grant gone")
	}
	// A subscription that isn't pneu's, or is another server's.
	pt = newPushTest(t)
	pt.firstInit()
	pt.f.SetSubscription("pneu-personal", "pneu-personal", `{"name":"projects/pneu-push-test/subscriptions/pneu-personal","topic":"projects/pneu-push-test/topics/pneu-personal","ackDeadlineSeconds":30,"messageRetentionDuration":"3600s","expirationPolicy":{},"labels":{"pneu-install":"ffffffffffffffff"}}`)
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "belongs to another pneu server") {
		t.Errorf("another server's: %v", err)
	}
	inst := pt.load().Install
	pt.f.SetSubscription("pneu-personal", "pneu-personal", `{"name":"projects/pneu-push-test/subscriptions/pneu-personal","topic":"projects/pneu-push-test/topics/pneu-personal","ackDeadlineSeconds":60,"messageRetentionDuration":"3600s","expirationPolicy":{},"labels":{"pneu-install":"`+inst+`"}}`)
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "its ackDeadlineSeconds isn't what pneu makes: delete it (gcloud pubsub subscriptions delete pneu-personal --project=pneu-push-test)") {
		t.Errorf("a different subscription: %v", err)
	}
	if _, ok := pt.load().Accounts["personal"]; ok {
		t.Error("committed with a mismatched subscription")
	}
}

// Every code has words, none of them empty or Google's.
func TestPushWordsCoverCodes(t *testing.T) {
	px := pushCtx{project: "p-roject", name: "personal", address: mailAddr, owner: ownerAddr}
	for _, op := range google.Ops {
		for _, code := range google.Codes {
			for _, kind := range []google.Kind{google.Owner, google.Mailbox} {
				msg := px.explain(&google.Error{Op: op, Code: code}, kind).Error()
				if !strings.Contains(msg, " failed: ") || strings.HasSuffix(msg, ": ") {
					t.Errorf("%s %s %s: %q", op, code, kind, msg)
				}
			}
		}
	}
	for _, r := range append(control.ReauthReasons, control.FailingReasons...) {
		if reasonWords(r, "personal") == "an unknown failure" && r != control.ReasonUnknown {
			t.Errorf("reason %s has no words", r)
		}
	}
}

// The mailbox chosen at the consent must be the configured one.
func TestPushOnWrongMailbox(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	pt.f.AddUser("other@example.com")
	pt.signInAs("other@example.com")
	err := pt.e.on("personal", false)
	if err == nil || !strings.Contains(err.Error(), "the mailbox chosen at Google's consent isn't me@example.com, so nothing was set up") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error()+pt.out.String(), "other@example.com") {
		t.Errorf("Google's answer named in the output")
	}
	if pt.f.HasTopic("pneu-personal") || len(pt.load().Accounts) != 0 {
		t.Fatal("provisioned or committed for the wrong mailbox")
	}
}

// Two accounts can't push one address.
func TestPushOnTwoAccountsOneAddress(t *testing.T) {
	pt := newPushTest(t, "personal", mailAddr, "work", "ME@example.com")
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	n := pt.consents()
	err := pt.e.on("work", false)
	if err == nil || !strings.Contains(err.Error(), "ME@example.com is already pushed as account personal") {
		t.Fatalf("%v", err)
	}
	if pt.consents() != n || pt.f.HasTopic("pneu-work") {
		t.Fatal("went on after the refusal")
	}
}

// A denial, a callback that isn't this consent's, and a replayed one.
func TestPushConsentCallbacks(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	pt.mu.Lock()
	pt.browse = func(u string, port int) {
		q, _ := pt.f.Deny(u)
		redirect(port, q)
	}
	pt.mu.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "consent wasn't given") {
		t.Fatalf("denied: %v", err)
	}

	// A page that doesn't know the state gets a 400 and doesn't use up the
	// one-shot; the real answer then goes through. Its query is kept to
	// replay later.
	var kept string
	pt.mu.Lock()
	pt.browse = func(u string, port int) {
		q, _ := pt.f.Approve(u, mailAddr)
		v, _ := url.ParseQuery(q)
		forged := url.Values{"state": {"x" + v.Get("state")}, "code": {v.Get("code")}}
		if code, body := redirect(port, forged.Encode()); code != http.StatusBadRequest || body != relayRefused {
			pt.errs <- fmt.Errorf("forged state: %d %q", code, body)
		}
		kept = q
		if code, body := redirect(port, q); code != http.StatusOK || body != pushDone {
			pt.errs <- fmt.Errorf("real answer: %d %q", code, body)
		}
		// One-shot: the same answer again is refused.
		if code, _ := redirect(port, q); code == http.StatusOK {
			pt.errs <- errors.New("a second callback was taken")
		}
	}
	pt.mu.Unlock()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatalf("with a forged callback first: %v\n%s", err, pt.out.String())
	}
	// A replay into the next consent: its state is that consent's, not
	// this one's, so it's refused, and the consent waits for its own.
	pt.mu.Lock()
	pt.browse = func(u string, port int) {
		if code, _ := redirect(port, kept); code != http.StatusBadRequest {
			pt.errs <- fmt.Errorf("replay: %d", code)
		}
		q, _ := pt.f.Approve(u, mailAddr)
		redirect(port, q)
	}
	pt.mu.Unlock()
	if err := pt.e.on("personal", true); err != nil {
		t.Fatalf("with a replay first: %v", err)
	}
	close(pt.errs)
	for err := range pt.errs {
		t.Error(err)
	}
}

// The server's consent binds the port the way lieer does, and holds it:
// lieer's consent waiting there (or a connection still closing) refuses
// it with words, before any URL is shown.
func TestPushConsentPortBusy(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	port := freeBothPort(t)
	lieer, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer lieer.Close()
	tp := pt.e.out.(termPush)
	tp.listen = func() ([]net.Listener, error) { return listenLoopback(port, bindExclusive) }
	pt.e.out = tp
	pt.out.Reset()
	err = pt.e.on("personal", false)
	if err == nil || !strings.Contains(err.Error(), "a consent is already waiting on localhost:8080 (lieer's or pneu's)") {
		t.Fatalf("%v", err)
	}
	if pt.consents() != 1 || strings.Contains(pt.out.String(), "Opening Google's consent screen in your browser") {
		t.Fatal("a URL was shown with the port taken")
	}
}

// bindExclusive fails on a TIME_WAIT left by a consent's redirect, as
// lieer's bind does; bindReuse (the client relay's) doesn't.
func TestBindPolicyTimeWait(t *testing.T) {
	port := freeBothPort(t)
	lns, err := listenLoopback(port, bindReuse)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", lns[0].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := lns[0].Accept()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lns {
		l.Close()
	}
	server.Close() // the server closes first, as an HTTP server does
	client.Close()
	time.Sleep(50 * time.Millisecond)
	if lns, err := listenLoopback(port, bindExclusive); !errors.Is(err, errPortBusy) {
		for _, l := range lns {
			l.Close()
		}
		t.Fatalf("exclusive bind over TIME_WAIT: %v", err)
	}
	lns, err = listenLoopback(port, bindReuse)
	if err != nil {
		t.Fatalf("reuse bind over TIME_WAIT: %v", err)
	}
	for _, l := range lns {
		l.Close()
	}
}

// The hand-over's outcomes: a mismatch is re-read and retried; stale and
// an error reply are pending; no socket answering is "no daemon" only
// when daemon.lock is free; a pneu without push is told to restart.
func TestPushHandOver(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if !strings.Contains(pt.out.String(), "pneu isn't running; it takes this when it starts.") {
		t.Errorf("no daemon:\n%s", pt.out.String())
	}
	// Someone holds daemon.lock but no socket answers: pending.
	dl, err := pt.e.store.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	err = pt.e.on("personal", false)
	dl.Unlock()
	if err == nil || !strings.Contains(err.Error(), "its push sync holds its lock, but its control socket didn't take the reload") {
		t.Fatalf("lock held, no socket: %v", err)
	}
	// No runtime dir at all: the same rule.
	pt.e.sockErr = control.ErrNoRuntimeDir
	pt.out.Reset()
	if err := pt.e.on("personal", false); err != nil || !strings.Contains(pt.out.String(), "pneu isn't running; instant mail for personal starts when it does.") {
		t.Fatalf("no runtime dir: %v\n%s", err, pt.out.String())
	}
	pt.e.sockErr = nil
	// A pneu answering without push.
	s, err := control.Listen(pt.e.socket, control.Handler{})
	if err != nil {
		t.Fatal(err)
	}
	pt.out.Reset()
	if err := pt.e.on("personal", false); err != nil || !strings.Contains(pt.out.String(), "runs no push sync; restart it") {
		t.Fatalf("push off: %v\n%s", err, pt.out.String())
	}
	s.Close()

	d := startStubDaemon(t, pt.e)
	// One mismatch, then the real answer.
	first := true
	d.mu.Lock()
	d.reload = func(gen uint64, hash string) (control.Reload, error) {
		if first {
			first = false
			return control.ReloadMismatch, nil
		}
		return d.apply(gen, hash)
	}
	d.mu.Unlock()
	n := d.reloadCount()
	if err := pt.e.on("personal", false); err != nil || d.reloadCount() != n+2 {
		t.Fatalf("mismatch then ok: %v, reloads %d", err, d.reloadCount()-n)
	}
	// Mismatch every time: pending after reloadTries.
	d.mu.Lock()
	d.reload = func(uint64, string) (control.Reload, error) { return control.ReloadMismatch, nil }
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "kept finding a different state.json") {
		t.Fatalf("mismatch forever: %v", err)
	}
	// Stale.
	d.mu.Lock()
	d.reload = func(uint64, string) (control.Reload, error) { return control.ReloadStale, nil }
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "applied a newer generation") {
		t.Fatalf("stale: %v", err)
	}
}

// A socket that takes the line and closes without an answer is no ack.
func TestPushHandOverNoAnswer(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := os.MkdirAll(filepath.Dir(pt.e.socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", pt.e.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Read(make([]byte, 256))
			c.Close()
		}
	}()
	dl, err := pt.e.store.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "no acknowledgment from it): pending") {
		t.Fatalf("%v", err)
	}
}

// The wait for delivering: only this generation counts; reauth stops it
// with the fix; nothing in time says what's unproven, and succeeds.
func TestPushAwaitDelivery(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	calls := 0
	d.mu.Lock()
	d.state = func(account string, gen uint64) control.PushState {
		calls++
		if calls < 3 { // the previous generation's word doesn't count
			return control.PushState{Instance: stubInstance, Generation: gen - 1, State: control.PushDelivering}
		}
		return control.PushState{Instance: stubInstance, Generation: gen, State: control.PushStarting}
	}
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pt.out.String(), "Instant mail is set up for personal, but not proven: no notification reached pneu within 2s (it reports starting). Everything stays in place, and running pneu account push personal again is safe.") {
		t.Errorf("timeout:\n%s", pt.out.String())
	}
	d.mu.Lock()
	d.state = func(account string, gen uint64) control.PushState {
		return control.PushState{Instance: stubInstance, Generation: gen, State: control.PushReauth, Reason: control.ReasonMailboxReauth}
	}
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "run pneu account push personal --reconsent") {
		t.Fatalf("reauth: %v", err)
	}
	d.mu.Lock()
	d.state = func(account string, gen uint64) control.PushState {
		return control.PushState{Instance: stubInstance, Generation: gen, State: control.PushFailing, Reason: control.ReasonAPIDisabled}
	}
	d.mu.Unlock()
	pt.out.Reset()
	if err := pt.e.on("personal", false); err != nil || !strings.Contains(pt.out.String(), "(it reports failing: an API isn't enabled in the push project)") {
		t.Fatalf("failing: %v\n%s", err, pt.out.String())
	}
}

// --off: off-pending, the ack, users.stop, the removal; what to delete
// and where to revoke, with the owner caveat only when it applies.
func TestPushOff(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	pt.out.Reset()
	n := d.reloadCount()
	if err := pt.e.off("personal"); err != nil {
		t.Fatalf("%v\n%s", err, pt.out.String())
	}
	if _, ok := pt.load().Accounts["personal"]; ok || pt.f.Stops(mailAddr) != 1 || d.reloadCount() != n+2 {
		t.Fatalf("after off: %+v, stops %d, reloads %d", pt.load().File, pt.f.Stops(mailAddr), d.reloadCount()-n)
	}
	if _, _, ok := pt.f.Watch(mailAddr); ok {
		t.Error("the watch is still there")
	}
	for _, want := range []string{
		"Instant mail is off for personal.",
		"gcloud pubsub subscriptions delete pneu-personal --project=pneu-push-test",
		"gcloud pubsub topics delete pneu-personal --project=pneu-push-test",
		"pneu never revokes a grant",
		"https://myaccount.google.com/permissions",
	} {
		if !strings.Contains(pt.out.String(), want) {
			t.Errorf("no %q in\n%s", want, pt.out.String())
		}
	}
	if strings.Contains(pt.out.String(), "also the push owner") {
		t.Error("the owner caveat for a mailbox that isn't the owner")
	}
	if !pt.f.HasTopic("pneu-personal") {
		t.Error("the topic was deleted")
	}
	// Again: already off.
	if err := pt.e.off("personal"); err != nil || !strings.Contains(pt.out.String(), "already off for personal") {
		t.Fatalf("again: %v", err)
	}

	// The owner is the mailbox: the caveat.
	pt2 := newPushTest(t, "own", ownerAddr)
	startStubDaemon(t, pt2.e)
	pt2.firstInit()
	pt2.signInAs(ownerAddr)
	if err := pt2.e.on("own", false); err != nil {
		t.Fatal(err)
	}
	if err := pt2.e.off("own"); err != nil || !strings.Contains(pt2.out.String(), "owner@gmail.com is also the push owner: revoking either grant there ends both") {
		t.Fatalf("owner caveat: %v\n%s", err, pt2.out.String())
	}
}

// --off interrupted at each step: no ack stops before anything is
// deleted; a failed stop stays off-pending; each rerun finishes.
func TestPushOffInterrupted(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	// No ack.
	d.mu.Lock()
	d.reload = func(uint64, string) (control.Reload, error) { return 0, errors.New("busy") }
	d.mu.Unlock()
	err := pt.e.off("personal")
	if err == nil || !strings.Contains(err.Error(), "pending; the daemon applies it when it answers. Nothing was deleted") {
		t.Fatalf("no ack: %v", err)
	}
	if a := pt.load().Accounts["personal"]; a.State != state.OffPending || pt.f.Stops(mailAddr) != 0 {
		t.Fatalf("no ack: %+v, stops %d", a, pt.f.Stops(mailAddr))
	}
	d.mu.Lock()
	d.reload = nil
	d.mu.Unlock()
	// The stop fails: stays off-pending, cleanup pending.
	pt.f.FailCode(google.OpStop, google.CodeUnavailable, 1)
	gen := pt.load().Generation
	err = pt.e.off("personal")
	if err == nil || !strings.Contains(err.Error(), "remote cleanup is pending. Run pneu account push personal --off again to retry; the watch also lapses by itself within 7 days") {
		t.Fatalf("stop failed: %v", err)
	}
	if a := pt.load().Accounts["personal"]; a.State != state.OffPending || pt.load().Generation != gen {
		t.Fatalf("stop failed: %+v", pt.load().File)
	}
	pt.noGoogleText()
	// The refresh fails the same way.
	pt.f.FailCode(google.OpRefresh, google.CodeNetwork, 1)
	if err := pt.e.off("personal"); err == nil || !strings.Contains(err.Error(), "couldn't reach Google") {
		t.Fatalf("refresh failed: %v", err)
	}
	// The rerun finishes.
	if err := pt.e.off("personal"); err != nil {
		t.Fatalf("rerun: %v\n%s", err, pt.out.String())
	}
	if _, ok := pt.load().Accounts["personal"]; ok || pt.f.Stops(mailAddr) != 1 {
		t.Fatal("not removed")
	}
	// Turning it on while off-pending is fine: on again.
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.reload = func(uint64, string) (control.Reload, error) { return 0, errors.New("busy") }
	d.mu.Unlock()
	pt.e.off("personal")
	d.mu.Lock()
	d.reload = nil
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err != nil || pt.load().Accounts["personal"].State != state.On {
		t.Fatalf("on from off-pending: %v", err)
	}
}

// --off with the mailbox's grant revoked: users.stop can't run, so it
// stays off-pending (D4), the output says the watch lapses by itself and
// how to clean up now; that way works.
func TestPushOffRevoked(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	pt.f.Revoke(mailAddr)
	err := pt.e.off("personal")
	if err == nil || !strings.Contains(err.Error(), "the mailbox's grant no longer works, so pneu can't stop it. The watch lapses by itself within 7 days") {
		t.Fatalf("%v", err)
	}
	if pt.load().Accounts["personal"].State != state.OffPending {
		t.Fatalf("%+v", pt.load().File)
	}
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	if err := pt.e.off("personal"); err != nil {
		t.Fatal(err)
	}
	if _, ok := pt.load().Accounts["personal"]; ok || pt.f.Stops(mailAddr) != 1 {
		t.Fatalf("cleanup: %+v, stops %d", pt.load().File, pt.f.Stops(mailAddr))
	}
}

// --off holds push.lock through users.stop: an enable can't commit (and
// start a watch the stop would end) in between.
func TestPushOffHoldsLockThroughStop(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	locked := make(chan error, 1)
	tp := pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "watch on me@example.com is stopped") {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			l, err := pt.e.store.Lock(ctx)
			if err == nil {
				l.Unlock()
			}
			locked <- err
		}
	}}
	if err := pt.e.off("personal"); err != nil {
		t.Fatal(err)
	}
	if err := <-locked; err == nil {
		t.Fatal("push.lock was free during users.stop")
	}
}

// An access token refused (401) is dropped, refreshed, and the call made
// once more, at every step that takes one.
func TestPushRetriesRefusedToken(t *testing.T) {
	for _, op := range []google.Op{google.OpProfile, google.OpTopicMake, google.OpGetPolicy, google.OpSetPolicy, google.OpSubMake} {
		pt := newPushTest(t)
		pt.firstInit()
		pt.f.FailCode(op, google.CodeUnauthenticated, 1)
		if err := pt.e.on("personal", false); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	pt := newPushTest(t)
	pt.f.FailCode(google.OpTopicList, google.CodeUnauthenticated, 1)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	pt.f.FailCode(google.OpStop, google.CodeUnauthenticated, 1)
	if err := pt.e.off("personal"); err != nil || pt.f.Stops(mailAddr) != 1 {
		t.Fatalf("off: %v, stops %d", err, pt.f.Stops(mailAddr))
	}
	// Twice in a row is a failure, in words.
	pt.f.FailCode(google.OpTopicList, google.CodeUnauthenticated, 2)
	if err := pt.init(initParams{}); err == nil || !strings.Contains(err.Error(), "Google refused the access token") {
		t.Fatalf("twice: %v", err)
	}
}

// Init finds client.json as it read it, or commits nothing: a rotated
// secret written meanwhile isn't overwritten with the stale one.
func TestPushInitClientChangedMeanwhile(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	rotated := []byte(strings.Replace(string(pushClientJSON(pt.f)), pt.f.Secret, "GOCSPX-rotated", 1))
	tp := pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "reaches project") {
			l, err := pt.e.store.Lock(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			l.WriteClient(rotated)
			l.Unlock()
		}
	}}
	err := pt.init(initParams{})
	if err == nil || !strings.Contains(err.Error(), "push's setup changed while this ran") {
		t.Fatalf("%v", err)
	}
	if c, _ := pt.e.store.LoadClient(); c.Secret != "GOCSPX-rotated" || pt.load().Generation != 1 {
		t.Fatalf("the rotated secret was overwritten, or a generation committed: %v", c)
	}
}

// A client.json left by an interrupted re-init that doesn't match the
// state: the next plain init says how to finish, and rerunning with the
// client finishes it.
func TestPushInitRecoversClientMismatch(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	l, err := pt.e.store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.WriteClient([]byte(strings.Replace(string(pushClientJSON(pt.f)), pt.f.ClientID, "999-other.apps.googleusercontent.com", 1)))
	l.Unlock()
	if err := pt.init(initParams{}); err == nil || !strings.Contains(err.Error(), "rerunning the last pneu push init (with its --client-secret) finishes it") {
		t.Fatalf("plain init: %v", err)
	}
	if err := pt.init(initParams{client: pushClientJSON(pt.f)}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if c, _ := pt.e.store.LoadClient(); c.ID != pt.f.ClientID {
		t.Fatalf("client %v", c)
	}
}

// --off with no daemon holds daemon.lock until the removal, and lets go.
func TestPushOffNoDaemon(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	held := make(chan bool, 1)
	tp := pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "watch on me@example.com is stopped") {
			running, _ := pt.e.store.DaemonRunning()
			held <- running
		}
	}}
	if err := pt.e.off("personal"); err != nil {
		t.Fatalf("%v\n%s", err, pt.out.String())
	}
	if !<-held {
		t.Error("daemon.lock wasn't held through users.stop")
	}
	if running, _ := pt.e.store.DaemonRunning(); running {
		t.Error("daemon.lock still held")
	}
	if err := pt.e.off("nobody"); err != nil || !strings.Contains(pt.out.String(), "already off for nobody") {
		t.Fatalf("an account never pushed: %v", err)
	}
	empty := newPushTest(t)
	if err := empty.e.off("personal"); err == nil || !strings.Contains(err.Error(), "push isn't set up here") {
		t.Fatalf("no state: %v", err)
	}
}

// sayHook sees every line the command says.
type sayHook struct {
	termPush
	hook func(string)
}

func (t termPush) base() termPush { return t }

func (s sayHook) say(format string, a ...any) {
	s.hook(fmt.Sprintf(format, a...))
	s.termPush.say(format, a...)
}

// A push before init, and an account not in the config.
func TestPushOnPreconditions(t *testing.T) {
	pt := newPushTest(t)
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "push isn't set up here: run pneu push init") {
		t.Fatalf("before init: %v", err)
	}
	pt.firstInit()
	if err := pt.e.on("nobody", false); err == nil || !strings.Contains(err.Error(), `no account "nobody"`) {
		t.Fatalf("unknown account: %v", err)
	}
	// client.json swapped for another client: refused.
	l, err := pt.e.store.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	l.WriteClient([]byte(strings.Replace(string(pushClientJSON(pt.f)), pt.f.ClientID, "999-other.apps.googleusercontent.com", 1)))
	l.Unlock()
	if err := pt.e.on("personal", false); err == nil || !strings.Contains(err.Error(), "isn't the client recorded at init") {
		t.Fatalf("swapped client: %v", err)
	}
}

// Credentials written meanwhile aren't overwritten with the ones this run
// read: the owner's at init, the account's at push.
func TestPushCredentialChangedMeanwhile(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	bump := func(modify func(f *state.File)) {
		l, err := pt.e.store.Lock(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		defer l.Unlock()
		if _, err := l.Update(func(f *state.File) error { modify(f); return nil }); err != nil {
			t.Error(err)
		}
	}
	tp := pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "reaches project") {
			bump(func(f *state.File) { f.Owner.Refresh = "1//newer-owner-grant" })
		}
	}}
	if err := pt.init(initParams{}); err == nil || !strings.Contains(err.Error(), "push's setup changed while this ran") {
		t.Fatalf("init: %v", err)
	}
	if pt.load().Owner.Refresh != "1//newer-owner-grant" {
		t.Fatal("the newer owner grant was overwritten")
	}

	pt = newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	tp = pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "subscription pneu-personal:") {
			bump(func(f *state.File) {
				a := f.Accounts["personal"]
				a.Refresh = "1//newer-mailbox-grant"
				f.Accounts["personal"] = a
			})
		}
	}}
	if err := pt.e.on("personal", true); err == nil || !strings.Contains(err.Error(), "personal's push state changed while this ran") {
		t.Fatalf("on: %v", err)
	}
	if pt.load().Accounts["personal"].Refresh != "1//newer-mailbox-grant" {
		t.Fatal("the newer mailbox grant was overwritten")
	}
}

// The daemon acknowledges off-pending, then exits before the removal's
// reload: daemon.lock, taken to decide "no daemon", is let go.
func TestPushOffDaemonGoesBetweenReloads(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	tp := pt.e.out.(termPush)
	pt.e.out = sayHook{termPush: tp, hook: func(line string) {
		if strings.Contains(line, "watch on me@example.com is stopped") {
			d.stop()
		}
	}}
	if err := pt.e.off("personal"); err != nil {
		t.Fatal(err)
	}
	if running, _ := pt.e.store.DaemonRunning(); running {
		t.Fatal("daemon.lock still held after --off")
	}
}

// cancelOut is a terminal whose context a hook can end mid-command, as a
// remote client's stdin closing does.
type cancelOut struct {
	termPush
	c    context.Context
	hook func(string)
}

func (o cancelOut) ctx() context.Context { return o.c }
func (o cancelOut) say(format string, a ...any) {
	o.hook(fmt.Sprintf(format, a...))
	o.termPush.say(format, a...)
}

// A command whose context ends after its last Google call (a remote
// client gone, with no consent to cancel) commits nothing.
func TestPushCancelledCommitsNothing(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	gen := pt.load().Generation
	for _, tc := range []struct {
		after string
		run   func() error
	}{
		{"subscription pneu-personal:", func() error { return pt.e.on("personal", false) }},
		{"reaches project", func() error { return pt.init(initParams{}) }},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		pt.e.out = cancelOut{termPush: pt.e.out.(interface{ base() termPush }).base(), c: ctx, hook: func(line string) {
			if strings.Contains(line, tc.after) {
				cancel()
			}
		}}
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), "nothing was committed") {
			t.Errorf("after %q: %v", tc.after, err)
		}
		cancel()
		if pt.load().Generation != gen {
			t.Fatalf("after %q: committed", tc.after)
		}
	}
}

// A restart during the wait weakens the proof: delivering then is worded
// as messages arriving, not as this watch's.
func TestPushAwaitDeliveryRestart(t *testing.T) {
	pt := newPushTest(t)
	d := startStubDaemon(t, pt.e)
	pt.firstInit()
	calls := 0
	d.mu.Lock()
	d.state = func(account string, gen uint64) control.PushState {
		calls++
		if calls == 1 {
			return control.PushState{Instance: control.Instance(), Generation: gen, State: control.PushStarting}
		}
		return control.PushState{Instance: "fedcba9876543210", Generation: gen, State: control.PushDelivering}
	}
	d.mu.Unlock()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pt.out.String(), "the pneu answering couldn't be confirmed as the one that took this generation, so that shows messages arrive, not that this watch sent them.") ||
		strings.Contains(pt.out.String(), "Gmail's watch delivered its first notification") {
		t.Fatalf("output:\n%s", pt.out.String())
	}
	// Restarted before the first answer: the instance asked before the
	// hand-over never answers push-state.
	pt.out.Reset()
	d.mu.Lock()
	d.state = func(account string, gen uint64) control.PushState {
		return control.PushState{Instance: "fedcba9876543210", Generation: gen, State: control.PushDelivering}
	}
	d.mu.Unlock()
	if err := pt.e.on("personal", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pt.out.String(), "(pneu restarted meanwhile)") || strings.Contains(pt.out.String(), "Gmail's watch delivered its first notification") {
		t.Fatalf("restart before the first answer:\n%s", pt.out.String())
	}
}
