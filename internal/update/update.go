package update

// `pneu update` (docs/client.md, "pneu update"; R15, N13): fast-forward
// this machine's checkout to its recorded remote branch, build the new
// binary beside the live one before anything live changes, swap it in,
// restart, and prove the new instance is ready, or roll every piece back
// and prove the old one is. Code comes only from the recorded remote (T6):
// never from the server, never from a page.
//
// Before the user confirms, the only programs run are git, on the recorded
// checkout, without lazy fetching (U1): nothing built from the target, and
// no Go toolchain (the live binary's build is read with debug/buildinfo;
// U2). After confirmation `go build` may fetch the toolchain the target's
// go.mod names, as any build of it would.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/peer"
)

// Files, in the directories Env names.
const (
	LockFile   = "update.lock" // LockDir: never replaced or removed
	RecordFile = "update.json" // StateDir: an update in flight
	binName    = "pneu"
	newPrefix  = ".pneu.new." // + the transaction id: the build, beside the live binary (rename is atomic)
	prevName   = "pneu.prev"  // the binary the last deploy replaced
	unit       = "pneu.service"
)

// Timing.
const (
	// ReadyWait bounds the wait for a restarted service to prove itself.
	ReadyWait = 20 * time.Second
	smokeWait = 10 * time.Second
	maxRecord = 8 << 10
	// maxIncoming bounds the commits listed before asking.
	maxIncoming = 40
)

// Record phases.
const (
	PhaseBuilt       = "built"        // the staged build is checked; nothing live touched
	PhaseDeploying   = "deploying"    // live files may be changing
	PhaseRollingBack = "rolling-back" // putting the old build back
)

// Record is update.json: the deployment in flight, written before anything
// live changes, so an interrupted update is found and finished or rolled
// back by the next `pneu update` (N13).
type Record struct {
	Phase       string `json:"phase"`
	Tx          string `json:"tx"` // the transaction: names the staged build
	Dir         string `json:"dir"`
	OldHead     string `json:"oldHead"`     // the checkout before
	OldRevision string `json:"oldRevision"` // the live binary's build ("" when it has no VCS stamp)
	OldModified bool   `json:"oldModified"`
	// OldLegacy: before the restart the daemon answered without
	// `listening`, a build from before the instance check. Only then may
	// a rollback verify the old build the legacy way (V1).
	OldLegacy   bool   `json:"oldLegacy"`
	OldSHA256   string `json:"oldSha256"`
	NewRevision string `json:"newRevision"` // the target: checkout and binary after
	NewSHA256   string `json:"newSha256"`
	Started     string `json:"started"`
}

// Env is everything pneu update touches; every program is run from argv
// with exec, never a shell, and each binary is a seam for tests.
type Env struct {
	Source config.Source
	Port   int  // the loopback port, for the readiness probe
	Client bool // a client: also keep the skew cache (Cache) current

	Git, Go, Systemctl string

	BinDir   string // where the live binary is: ~/.local/bin
	StateDir string // $XDG_STATE_HOME/pneu: update.json, skew.json
	LockDir  string // $XDG_RUNTIME_DIR/pneu: update.lock
	Socket   string // the control socket

	// Yes skips the question; Confirm asks it (an error: no terminal).
	Yes     bool
	Confirm func(question string) (bool, error)

	Ready, Poll time.Duration
	Out, Err    io.Writer
	Now         func() time.Time

	// lock is the held update.lock, handed to every child (child).
	lock *os.File
	// readCheckout, in tests, stands in for reading branch and HEAD.
	readCheckout func() checkout
	// crash, in tests, stops the run at a named point as if the process
	// had died there: nothing after it runs, nothing is cleaned up.
	crash func(point string) bool
}

// errCrash is crash's: the run stopped there.
var errCrash = errors.New("update: stopped (test crash)")

func (e *Env) at(point string) error {
	if e.crash != nil && e.crash(point) {
		return errCrash
	}
	return nil
}

func (e *Env) say(format string, args ...any) { fmt.Fprintf(e.Out, format+"\n", args...) }

func (e *Env) live() string                { return filepath.Join(e.BinDir, binName) }
func (e *Env) staged(tx string) string     { return filepath.Join(e.BinDir, newPrefix+tx) }
func (e *Env) stagedOf(rec *Record) string { return e.staged(rec.Tx) }
func (e *Env) prevBin() string             { return filepath.Join(e.BinDir, prevName) }
func (e *Env) record() string              { return filepath.Join(e.StateDir, RecordFile) }
func (e *Env) cache() string               { return filepath.Join(e.StateDir, CacheFile) }

// ErrLocked: another pneu update holds the lock, or a program one started
// (a build, a fetch) still runs after it died.
var ErrLocked = errors.New("another pneu update is running (or a build or git it started is still running)")

// Lock takes the exclusive, non-blocking flock on LockDir/update.lock: a
// file never replaced or removed, 0600 and ours, in a 0700 directory of
// ours (the control socket's rules). Unlock by closing it.
func Lock(dir string) (*os.File, error) {
	if err := peer.PrepareDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	fi, err := f.Stat()
	if err == nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !fi.Mode().IsRegular():
			err = fmt.Errorf("update: %s isn't a regular file", path)
		case !ok || int(st.Uid) != os.Getuid():
			err = fmt.Errorf("update: %s isn't ours", path)
		case fi.Mode().Perm() != 0o600:
			err = fmt.Errorf("update: %s has mode %v, want 0600", path, fi.Mode().Perm())
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("update: lock: %w", err)
	}
	return f, nil
}

func (e *Env) takeLock() error {
	f, err := Lock(e.LockDir)
	if err != nil {
		return err
	}
	e.lock = f
	return nil
}

func (e *Env) dropLock() {
	if e.lock != nil {
		e.lock.Close()
		e.lock = nil
	}
}

// Run is `pneu update`: an interrupted update first (finished or rolled
// back, then it stops), else the whole transaction.
func (e *Env) Run() error {
	if err := e.takeLock(); err != nil {
		return err
	}
	defer e.dropLock()
	rec, err := e.loadRecord()
	if err != nil {
		return err
	}
	if rec != nil {
		return e.recover(rec)
	}
	return e.update()
}

// Check is `pneu update --check`: fetch, say what's incoming, and on a
// client record which build is older for the daemon (skew.json).
func (e *Env) Check() error {
	if err := e.takeLock(); err != nil {
		return err
	}
	defer e.dropLock()
	if rec, err := e.loadRecord(); err != nil {
		return err
	} else if rec != nil {
		e.say("An update was interrupted (%s): run pneu update to finish or roll it back.", rec.Phase)
	}
	if err := e.checkRemote(); err != nil {
		return err
	}
	target, err := e.fetch()
	if err != nil {
		return err
	}
	e.at("fetched")
	if e.Client {
		e.refreshSkew()
	}
	// What the checkout is now, after the fetch: one reading, branch
	// included (W1).
	c := e.snapshot()
	if c.head == "" || c.branch != e.Source.Branch {
		e.say("%s/%s is at %s, but %s, not on %s: pneu update would refuse.", e.Source.Remote, e.Source.Branch, short(target), c, e.Source.Branch)
		return fmt.Errorf("the checkout isn't on its recorded branch %s: %s", e.Source.Branch, c)
	}
	n, err := e.count(c.head, target)
	switch {
	case err != nil:
		e.say("%s/%s is at %s; %s, which isn't behind it in a straight line.", e.Source.Remote, e.Source.Branch, short(target), c)
		return fmt.Errorf("the checkout's HEAD %s isn't an ancestor of %s/%s", short(c.head), e.Source.Remote, e.Source.Branch)
	case n == 0:
		e.say("The checkout is at %s/%s (%s).", e.Source.Remote, e.Source.Branch, short(target))
	default:
		e.say("%d commit(s) on %s/%s to update to: run pneu update.", n, e.Source.Remote, e.Source.Branch)
	}
	return nil
}

// update is the transaction, docs/client.md "pneu update" steps 2–10.
func (e *Env) update() error {
	// Holding the lock with no record: nothing else is building, so a
	// staged build left by a dead run is litter.
	e.sweepStaged()
	if err := e.checkRemote(); err != nil {
		return err
	}
	if err := e.checkTree(); err != nil {
		return err
	}
	head, err := e.rev("HEAD")
	if err != nil {
		return err
	}
	target, err := e.fetch()
	if err != nil {
		return err
	}
	if err := e.at("fetched"); err != nil {
		return err
	}
	if ok, err := e.ancestor(head, target); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s/%s (%s) isn't a fast-forward of the checkout's HEAD (%s): history was rewritten, or the checkout has commits of its own. pneu update only fast-forwards; sort the checkout out by hand",
			e.Source.Remote, e.Source.Branch, short(target), short(head))
	}
	old, err := Identify(e.live())
	if err != nil {
		return fmt.Errorf("the live binary %s: %w (install it per INSTALL.md step 2 first)", e.live(), err)
	}
	incoming, n, err := e.incoming(head, target)
	if err != nil {
		return err
	}
	if n == 0 && old.Revision == target && !old.Modified {
		// Read again after the fetch: the claim is about the checkout now.
		if _, err := e.finalCheckout(target); err != nil {
			return err
		}
		e.say("Already up to date: the checkout and %s are at %s (%s/%s).", e.live(), short(target), e.Source.Remote, e.Source.Branch)
		if e.Client {
			e.refreshSkew()
		}
		return nil
	}
	// Which instance runs now: the restarted one must be another. A
	// daemon that answers but can't be read is no baseline to judge by.
	base, err := e.baseline()
	if err != nil {
		return fmt.Errorf("the running pneu answers its control socket, but not as expected (%v): look at systemctl --user status pneu before updating", err)
	}
	if n > 0 {
		e.say("%d commit(s) from %s/%s:\n%s", n, e.Source.Remote, e.Source.Branch, incoming)
	} else {
		e.say("The checkout is at %s/%s already, but %s is %s.", e.Source.Remote, e.Source.Branch, e.live(), describe(old))
	}
	e.say("This builds %s, replaces %s (the old one stays as %s), and restarts pneu.", short(target), e.live(), prevName)
	if !e.Yes {
		if e.Confirm == nil {
			return errors.New("no terminal to confirm on: run pneu update in a terminal, or pass --yes")
		}
		ok, err := e.Confirm("Update? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			e.say("Nothing changed.")
			return nil
		}
	}

	// Build before touching anything live.
	tx, err := newTx()
	if err != nil {
		return err
	}
	newSum, err := e.build(target, e.staged(tx))
	if err != nil {
		os.Remove(e.staged(tx))
		return fmt.Errorf("%w; nothing live was changed", err)
	}
	oldSum, err := fileSHA(e.live())
	if err != nil {
		os.Remove(e.staged(tx))
		return err
	}
	rec := &Record{
		Phase: PhaseBuilt, Tx: tx, Dir: e.Source.Dir, OldHead: head, OldRevision: old.Revision, OldModified: old.Modified,
		OldLegacy: base.legacy, OldSHA256: oldSum, NewRevision: target, NewSHA256: newSum, Started: e.now().UTC().Format(time.RFC3339),
	}
	if err := e.saveRecord(rec); err != nil {
		os.Remove(e.staged(tx))
		return err
	}
	if err := e.at("recorded"); err != nil {
		return err
	}
	// Nothing else takes the lock: an editor or a hand-run git may have
	// moved the checkout while it built.
	if err := e.recheck(head); err != nil {
		os.Remove(e.staged(tx))
		if rerr := e.removeRecord(); rerr != nil {
			return fmt.Errorf("%w; nothing live was changed, but %v", err, rerr)
		}
		return fmt.Errorf("%w; nothing live was changed", err)
	}
	if e.Client {
		// The daemon about to start reads which side is older at once.
		e.recordSkew(target)
	}
	return e.deploy(rec, base.instance)
}

func newTx() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sweepStaged removes staged builds: only with the lock held, when no
// child of a dead run can still be writing one (they hold the lock too).
func (e *Env) sweepStaged() {
	ms, _ := filepath.Glob(filepath.Join(e.BinDir, newPrefix+"*"))
	for _, m := range ms {
		os.Remove(m)
	}
}

// deploy swaps the build in, fast-forwards the checkout and restarts,
// then verifies; any failure but the checkout's rolls back.
func (e *Env) deploy(rec *Record, before string) error {
	rec.Phase = PhaseDeploying
	if err := e.saveRecord(rec); err != nil {
		return e.rollback(rec, err)
	}
	if err := e.at("deploying"); err != nil {
		return err
	}
	if err := copyFile(e.live(), e.prevBin()); err != nil {
		return e.rollback(rec, fmt.Errorf("keeping the old binary as %s: %w", prevName, err))
	}
	if err := e.at("prev"); err != nil {
		return err
	}
	if err := e.swapIn(rec); err != nil {
		return e.rollback(rec, err)
	}
	if err := e.at("swapped"); err != nil {
		return err
	}
	note := e.fastForward(rec)
	if err := e.at("merged"); err != nil {
		return err
	}
	if err := e.restart(); err != nil {
		return e.rollback(rec, err)
	}
	if err := e.at("restarted"); err != nil {
		return err
	}
	if err := e.verify(Build{rec.NewRevision, false}, before, false); err != nil {
		return e.rollback(rec, fmt.Errorf("the new build didn't come up: %w", err))
	}
	return e.finished(rec, "Updated", note)
}

// swapIn renames the staged build over the live binary, after checking it
// is still the build that was recorded (no other run's build or stray
// write slips in), and makes the rename durable.
func (e *Env) swapIn(rec *Record) error {
	if !sumIs(e.stagedOf(rec), rec.NewSHA256) {
		return fmt.Errorf("the staged build %s isn't the one that was built and checked", e.stagedOf(rec))
	}
	if err := os.Rename(e.stagedOf(rec), e.live()); err != nil {
		return err
	}
	if err := syncDir(e.BinDir); err != nil {
		return fmt.Errorf("making the swap durable: %w", err)
	}
	return nil
}

// fastForward moves the checkout to the target: "" when the merge ran, or
// what went wrong. Where the checkout ended is read again at the report
// (checkoutAt), never taken from here: a concurrent switch, a hook's doing
// or a failed merge is reported as it is then (U6, V3), never rolled back
// over.
func (e *Env) fastForward(rec *Record) string {
	if _, err := e.gitMut("merge", "--ff-only", "--quiet", rec.NewRevision); err != nil {
		return fmt.Sprintf("git merge --ff-only failed (%v)", err)
	}
	return ""
}

// checkoutAt reads the checkout now: whether it's on the recorded branch
// at rev, and where it is in words.
func (e *Env) checkoutAt(rev string) (string, bool) {
	c := e.snapshot()
	return c.String(), c.at(e.Source.Branch, rev)
}

// checkout is one reading of the checkout: its branch ("" detached or
// unreadable) and HEAD ("" unreadable). Verdict and words come from the
// same reading (W2).
type checkout struct{ branch, head string }

func (c checkout) at(branch, rev string) bool {
	return c.branch == branch && c.head != "" && c.head == rev
}

func (c checkout) String() string {
	if c.head == "" {
		return "the checkout's HEAD is unreadable"
	}
	where := "detached"
	if c.branch != "" {
		where = "on " + c.branch
	}
	return fmt.Sprintf("the checkout is %s at %s", where, short(c.head))
}

// snapshot reads branch and HEAD, twice, and again if the two differ (the
// checkout moving as it's read): the last reading is the report's.
func (e *Env) snapshot() checkout {
	read := e.readCheckout
	if read == nil {
		read = e.readGit
	}
	a, b := read(), read()
	if a != b {
		b = read()
	}
	return b
}

func (e *Env) readGit() checkout {
	{
		var c checkout
		if b, err := e.git("symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			c.branch = strings.TrimSpace(b)
		}
		if h, err := e.rev("HEAD"); err == nil {
			c.head = h
		}
		return c
	}
}

// finalCheckout is every exit's last word on the checkout (W1): read now,
// it must be on the recorded branch at want, or the exit is an error that
// says where it is.
func (e *Env) finalCheckout(want string) (string, error) {
	where, ok := e.checkoutAt(want)
	if !ok {
		return where, fmt.Errorf("the checkout isn't at %s on %s: %s (it changed while pneu update ran; left as it is)", short(want), e.Source.Branch, where)
	}
	return where, nil
}

// atTarget: HEAD is the recorded branch, at the target.
func (e *Env) atTarget(rec *Record) bool {
	b, err := e.git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(b) != e.Source.Branch {
		return false
	}
	h, err := e.rev("HEAD")
	return err == nil && h == rec.NewRevision
}

// checkoutState says where the checkout is, for a report.
func (e *Env) checkoutState() string { return e.snapshot().String() }

// finished clears the record durably and reports, reading the checkout
// at that moment (V3). A record that can't be cleared durably is said and
// kept as an error: the next pneu update checks again, which is harmless
// when everything is done (U4). A checkout that isn't on the recorded
// branch at the target is said as it is, and is an error.
func (e *Env) finished(rec *Record, verb, note string) error {
	if err := e.removeRecord(); err != nil {
		e.say("%s %s is at %s and pneu runs it, but update.json couldn't be cleared durably (%v): the next pneu update will check again.", verb, e.live(), short(rec.NewRevision), err)
		return err
	}
	where, ok := e.checkoutAt(rec.NewRevision)
	checkout := "at " + short(rec.NewRevision) + " on " + e.Source.Branch
	if !ok {
		checkout = fmt.Sprintf("NOT at %s on %s: %s (changed while pneu update ran; left as it is)", short(rec.NewRevision), e.Source.Branch, where)
		if note != "" {
			checkout += "; " + note
		}
	}
	e.say("%s: %s is at %s and pneu is running it. Checkout: %s.", verb, e.live(), short(rec.NewRevision), checkout)
	e.remind()
	if !ok {
		return fmt.Errorf("the binary is deployed, but the checkout isn't at %s on %s: %s", short(rec.NewRevision), e.Source.Branch, where)
	}
	return nil
}

// rollback puts the old build back as one transaction: the binary from
// pneu.prev, the checkout to the old HEAD only if it's still clean at the
// new one, a restart, and the same readiness check for the old build. It
// says where everything ended.
func (e *Env) rollback(rec *Record, why error) error {
	e.say("Rolling back: %v", why)
	rec.Phase = PhaseRollingBack
	if err := e.saveRecord(rec); err != nil {
		e.say("(couldn't record the rollback: %v)", err)
	}
	binary := ""
	switch {
	case sumIs(e.live(), rec.OldSHA256):
		binary = "is the old build"
	case sumIs(e.prevBin(), rec.OldSHA256):
		if err := os.Rename(e.prevBin(), e.live()); err != nil {
			return fmt.Errorf("rollback stopped: putting %s back: %v. %s is unchanged; update.json keeps the deployment for the next pneu update", prevName, err, e.live())
		}
		binary = "restored from " + prevName
	default:
		return fmt.Errorf("rollback stopped: neither %s nor %s is the old build (sha256 %s), so pneu update can't restore it; rebuild it by hand (INSTALL.md step 2). update.json keeps the record", e.live(), prevName, rec.OldSHA256[:12])
	}
	if err := syncDir(e.BinDir); err != nil {
		return fmt.Errorf("rollback stopped: %s %s, but its directory couldn't be synced (%v); update.json keeps the record for the next pneu update", e.live(), binary, err)
	}
	os.Remove(e.stagedOf(rec))
	if err := e.at("rollback-binary"); err != nil {
		return err
	}
	action := e.rollbackCheckout(rec)
	// The failed instance's id: with none readable the files are still
	// restored and restarted, but nothing can say the answer afterwards
	// came from a fresh start (V2).
	base, berr := e.baseline()
	service := "restarted and verified at the old build"
	if err := e.restart(); err != nil {
		service = fmt.Sprintf("NOT restarted (%v): systemctl --user restart pneu", err)
	} else if berr != nil {
		service = fmt.Sprintf("restarted, but restart freshness couldn't be verified: before the restart the control socket answered out of shape (%v); systemctl --user status pneu", berr)
	} else if err := e.verify(Build{rec.OldRevision, rec.OldModified}, base.instance, rec.OldLegacy); err != nil {
		service = fmt.Sprintf("restarted, but NOT verified at the old build (%v): systemctl --user status pneu", err)
	}
	record := ""
	if err := e.removeRecord(); err != nil {
		record = fmt.Sprintf(" update.json couldn't be cleared durably (%v): the next pneu update will finish the rollback again.", err)
	}
	// Where the checkout is now, read now (V3).
	where, ok := e.checkoutAt(rec.OldHead)
	checkout := action + "; now " + where
	if !ok {
		checkout += fmt.Sprintf(", NOT at the old revision %s on %s", short(rec.OldHead), e.Source.Branch)
	}
	e.say("Rolled back. Binary: %s (%s). Checkout: %s. Service: %s.%s", e.live(), binary, checkout, service, record)
	return fmt.Errorf("update failed and was rolled back: %v", why)
}

// rollbackCheckout moves the checkout back to the old HEAD only if it's
// clean, on the recorded branch and exactly at the new revision; anything
// else is left as found.
func (e *Env) rollbackCheckout(rec *Record) string {
	head, err := e.rev("HEAD")
	if err != nil {
		return fmt.Sprintf("left as it is (HEAD unreadable: %v)", err)
	}
	if head == rec.OldHead {
		if _, ok := e.checkoutAt(rec.OldHead); !ok {
			return fmt.Sprintf("LEFT as found: at the old revision, but not on %s", e.Source.Branch)
		}
		return "at the old revision " + short(head)
	}
	clean, err := e.clean()
	if !e.atTarget(rec) || err != nil || !clean {
		return fmt.Sprintf("LEFT as found (%s), because it isn't clean at the new revision on %s; to put it back yourself: git -C %s reset --keep %s",
			e.checkoutState(), e.Source.Branch, config.ShellWord(e.Source.Dir), rec.OldHead)
	}
	if _, err := e.gitMut("reset", "--keep", "--quiet", rec.OldHead); err != nil {
		return fmt.Sprintf("LEFT at %s (git reset failed: %v)", short(head), err)
	}
	if where, ok := e.checkoutAt(rec.OldHead); !ok {
		return "reset to the old revision ran, but the checkout then was " + where
	}
	return "reset to the old revision " + short(rec.OldHead)
}

// recover deals with an update.json left by an interrupted update, then
// stops: run pneu update again for anything new.
func (e *Env) recover(rec *Record) error {
	if rec.Dir != e.Source.Dir {
		return fmt.Errorf("update.json is for the checkout %s, but the config's source is %s: finish or roll that back by hand, then remove %s", rec.Dir, e.Source.Dir, e.record())
	}
	e.say("An update from %s to %s was interrupted (%s).", short(rec.OldRevision), short(rec.NewRevision), rec.Phase)
	switch rec.Phase {
	case PhaseBuilt:
		// Nothing live was touched yet.
		e.sweepStaged()
		if err := e.removeRecord(); err != nil {
			return err
		}
		where, err := e.finalCheckout(rec.OldHead)
		e.say("Nothing had been deployed; cleaned up (%s). Run pneu update again.", where)
		return err
	case PhaseRollingBack:
		return e.rollback(rec, errors.New("finishing an interrupted rollback"))
	}
	// Deploying: whatever got done, finish it, or roll back.
	liveOld, liveNew := sumIs(e.live(), rec.OldSHA256), sumIs(e.live(), rec.NewSHA256)
	head, err := e.rev("HEAD")
	if err != nil {
		return err
	}
	if liveOld && head == rec.OldHead {
		e.sweepStaged()
		if err := e.removeRecord(); err != nil {
			return err
		}
		where, err := e.finalCheckout(rec.OldHead)
		e.say("Nothing live had changed yet; cleaned up (%s). Run pneu update again.", where)
		return err
	}
	if !liveNew {
		return e.rollback(rec, errors.New("the live binary isn't the new build"))
	}
	if err := syncDir(e.BinDir); err != nil {
		return e.rollback(rec, fmt.Errorf("making the swap durable: %w", err))
	}
	// The instance before the restart, readable, or recovery stops here:
	// without it no answer afterwards proves a fresh start (V2).
	base, err := e.baseline()
	if err != nil {
		return fmt.Errorf("recovery stopped: the running pneu answers its control socket out of shape (%v), so a restart couldn't be told from the process running now. update.json is kept; look at systemctl --user status pneu, then run pneu update again", err)
	}
	note := ""
	switch {
	case e.atTarget(rec):
	case head == rec.OldHead:
		b, berr := e.git("symbolic-ref", "--quiet", "--short", "HEAD")
		clean, cerr := e.clean()
		if berr != nil || strings.TrimSpace(b) != e.Source.Branch || cerr != nil || !clean {
			return e.rollback(rec, errors.New("the checkout changed while the update was interrupted"))
		}
		note = e.fastForward(rec)
	default:
		// Deployed, and the checkout moved by hand since: finished
		// reports where it is, never calls it updated.
		note = "it moved while the update was interrupted"
	}
	if err := e.restart(); err != nil {
		return e.rollback(rec, err)
	}
	if err := e.verify(Build{rec.NewRevision, false}, base.instance, false); err != nil {
		return e.rollback(rec, fmt.Errorf("the new build didn't come up: %w", err))
	}
	return e.finished(rec, "Finished", note)
}

func (e *Env) remind() {
	e.say("The bar widget runs from the checkout: its changes show after a shell restart (omarchy-restart-shell), which pneu update doesn't do.")
}

// --- checks ---

// checkRemote refuses a remote that now points somewhere other than the
// URL recorded with the source: code comes only from that (T6).
func (e *Env) checkRemote() error {
	url, err := e.git("remote", "get-url", "--", e.Source.Remote)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(url); got != e.Source.URL {
		return fmt.Errorf("the remote %s is now %q, not %q as recorded: if that's intended, record it again with pneu source set", e.Source.Remote, got, e.Source.URL)
	}
	return nil
}

// checkTree refuses a dirty tree, or HEAD off the recorded branch.
func (e *Env) checkTree() error {
	clean, err := e.clean()
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("the checkout %s has uncommitted changes (git status): commit or stash them first", e.Source.Dir)
	}
	b, err := e.git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(b) != e.Source.Branch {
		return fmt.Errorf("the checkout %s isn't on its recorded branch %s: git switch %s", e.Source.Dir, e.Source.Branch, e.Source.Branch)
	}
	return nil
}

// recheck is checkTree plus HEAD where it was before the build.
func (e *Env) recheck(head string) error {
	if err := e.checkTree(); err != nil {
		return err
	}
	if now, err := e.rev("HEAD"); err != nil || now != head {
		return errors.New("the checkout's HEAD moved while pneu update was building")
	}
	return nil
}

func (e *Env) clean() (bool, error) {
	out, err := e.git("status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// fetch gets the recorded branch into its remote-tracking ref and resolves
// it once: the one target everything after uses.
func (e *Env) fetch() (string, error) {
	ref := "refs/remotes/" + e.Source.Remote + "/" + e.Source.Branch
	cmd := e.gitCmd(false, "fetch", "--no-tags", "--quiet", "--end-of-options", e.Source.Remote, "+refs/heads/"+e.Source.Branch+":"+ref)
	// The network fetch doesn't inherit the lock (W3): it may start a
	// credential helper's daemon (credential-cache--daemon) that lives on,
	// and would hold the lock with it. Safe: an orphaned fetch after a
	// crash can only move this remote-tracking ref, and every run
	// re-resolves the target and rechecks ancestry under the lock.
	cmd.ExtraFiles = nil
	cmd.Stdout, cmd.Stderr = e.Out, e.Err
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git fetch %s %s: %w", e.Source.Remote, e.Source.Branch, err)
	}
	return e.rev(ref)
}

func (e *Env) rev(name string) (string, error) {
	out, err := e.git("rev-parse", "--verify", "--quiet", "--end-of-options", name+"^{commit}")
	r := strings.TrimSpace(out)
	if err != nil || !ValidRevision(r) {
		return "", fmt.Errorf("git: no commit %s in %s", name, e.Source.Dir)
	}
	return r, nil
}

// ancestor is `git merge-base --is-ancestor a b`: exit 0 yes, 1 no, else
// an error. Never a lazy fetch: a commit the checkout lacks is an error.
func (e *Env) ancestor(a, b string) (bool, error) {
	err := e.gitCmd(true, "merge-base", "--is-ancestor", a, b).Run()
	var xe *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &xe) && xe.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

func (e *Env) count(a, b string) (int, error) {
	if ok, err := e.ancestor(a, b); err != nil || !ok {
		return 0, errors.New("not a fast-forward")
	}
	out, err := e.git("rev-list", "--count", a+".."+b)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// incoming is the commits a..b as `git log --oneline` says them, at most
// maxIncoming, with anything a terminal would act on dropped.
func (e *Env) incoming(a, b string) (string, int, error) {
	n, err := e.count(a, b)
	if err != nil || n == 0 {
		return "", n, err
	}
	out, err := e.git("log", "--no-decorate", "--format=%h %s", "-n", strconv.Itoa(maxIncoming), a+".."+b)
	if err != nil {
		return "", 0, err
	}
	lines := strings.Split(strings.TrimRight(terminalSafe(out), "\n"), "\n")
	if n > len(lines) {
		lines = append(lines, fmt.Sprintf("… and %d more", n-len(lines)))
	}
	return "  " + strings.Join(lines, "\n  "), n, nil
}

// terminalSafe drops control characters but newlines: commit subjects go
// to a terminal.
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// --- build ---

// build checks the target out in a temporary worktree, builds it into out
// beside the live binary (same filesystem: the rename is atomic), syncs
// it, and runs its version as a smoke check: after trust is settled (the
// recorded remote, a fast-forward, the user's yes), not as trust. The
// worktree is removed whatever happens.
func (e *Env) build(target, out string) (string, error) {
	tmp, err := os.MkdirTemp("", "pneu-update-")
	if err != nil {
		return "", err
	}
	src := filepath.Join(tmp, "src")
	defer func() {
		e.gitMut("worktree", "remove", "--force", src)
		os.RemoveAll(tmp)
		e.gitMut("worktree", "prune")
	}()
	if _, err := e.gitMut("worktree", "add", "--detach", "--quiet", src, target); err != nil {
		return "", err
	}
	e.say("Building %s…", short(target))
	cmd := e.child(e.Go, "build", "-o", out, "./cmd/pneu")
	cmd.Dir = src
	cmd.Stdout, cmd.Stderr = e.Out, e.Err
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build: %w", err)
	}
	if err := fsyncFile(out); err != nil {
		return "", fmt.Errorf("syncing the build: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), smokeWait)
	defer cancel()
	smoke := e.child(out, "version")
	smoke.Dir = tmp
	o, err := runOutput(ctx, smoke)
	if want := "pneu " + target + "\n"; err != nil || o != want {
		return "", fmt.Errorf("the new build's smoke check failed: `version` said %q (%v), want %q", strings.TrimSpace(terminalSafe(o)), err, strings.TrimSpace(want))
	}
	return fileSHA(out)
}

// runOutput runs cmd for its stdout, killed at ctx's end.
func runOutput(ctx context.Context, cmd *exec.Cmd) (string, error) {
	var b strings.Builder
	cmd.Stdout = &b
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return b.String(), err
	case <-ctx.Done():
		cmd.Process.Kill()
		<-done
		return b.String(), ctx.Err()
	}
}

// Identify reads a Go binary's build (vcs.revision, vcs.modified) from the
// file with debug/buildinfo: nothing is run, so no toolchain is fetched or
// executed before the user confirms (U2), and a live binary from before
// `pneu version` reads the same.
func Identify(path string) (Build, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return Build{}, err
	}
	var b Build
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if ValidRevision(s.Value) {
				b.Revision = s.Value
			}
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b, nil
}

func describe(b Build) string {
	switch {
	case b.Revision == "":
		return "a build with no revision stamp"
	case b.Modified:
		return short(b.Revision) + " built from a modified tree"
	}
	return short(b.Revision)
}

// --- the service ---

func (e *Env) restart() error {
	e.say("Restarting pneu (a sync in progress finishes first)…")
	cmd := e.child(e.Systemctl, "--user", "restart", unit)
	cmd.Stdout, cmd.Stderr = e.Out, e.Err
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user restart %s: %w", unit, err)
	}
	return nil
}

// baseline is what the control socket says before a restart: absent
// (nothing answers: any instance after is a fresh one), or the instance
// and whether it answered legacy-style; an error when something answers
// out of shape (U5, V2). Every caller decides what that error stops.
type baseline struct {
	instance string
	absent   bool
	legacy   bool
}

func (e *Env) baseline() (baseline, error) {
	in, err := control.AskStatus(e.Socket)
	if errors.Is(err, control.ErrNotRunning) {
		return baseline{absent: true}, nil
	}
	if err != nil {
		return baseline{}, err
	}
	return baseline{instance: in.Instance, legacy: in.Legacy}, nil
}

// verify waits up to Ready for readiness, not just liveness (N13, U5): the
// control socket answers status with want's build, listening, and an
// instance other than before (a process started after the restart), and
// the loopback HTTP listener answers as pneu carrying that same instance
// in Pneu-Instance, so an old process still holding the port can't pass.
//
// allowLegacy is only ever true for the old build in a rollback, when the
// record says that build's daemon answered legacy-style at the baseline
// (V1): a target that answers without `listening` is not ready.
func (e *Env) verify(want Build, before string, allowLegacy bool) error {
	ready, poll := e.Ready, e.Poll
	if ready == 0 {
		ready = ReadyWait
	}
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	deadline := e.now().Add(ready)
	last := errors.New("no answer yet")
	for {
		e.at("verify") // a test seam: something may move under the wait
		in, err := control.AskStatus(e.Socket)
		switch {
		case err != nil:
			last = err
		case in.Instance == before:
			last = errors.New("the control socket still answers as the instance from before the restart")
		case in.Revision != want.Revision || in.Modified != want.Modified:
			last = fmt.Errorf("the control socket says it runs %s, want %s", describe(Build{in.Revision, in.Modified}), describe(want))
		case in.Legacy && !allowLegacy:
			return fmt.Errorf("the restarted pneu answers like a build from before the instance check (no listening), which %s isn't", describe(want))
		case !in.Listening && !in.Legacy:
			last = errors.New("the restarted pneu hasn't bound its HTTP listeners yet")
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			got, err := ProbeHTTP(ctx, e.Port)
			cancel()
			switch {
			case err != nil:
				last = err
			case in.Legacy:
				// The old build in a rollback, recorded as legacy at the
				// baseline: it can't tie its HTTP to its process, so a new
				// instance at its build plus pneu's HTTP answer has to do,
				// and the report says so.
				e.say("(%s predates the instance check: verified by its new instance and pneu's HTTP answer only)", describe(want))
				return nil
			case got != in.Instance:
				last = fmt.Errorf("loopback HTTP answers as instance %s, not the restarted %s (an old process holding the port?)", got, in.Instance)
			default:
				return nil
			}
		}
		if !e.now().Before(deadline) {
			return fmt.Errorf("not ready after %v: %w", ready, last)
		}
		time.Sleep(poll)
	}
}

var instanceRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ProbeHTTP asks the loopback listener on port which process it is,
// without a credential: a HEAD / with pneu's Host and no cookie gets
// Auth's 403, and every answer pneu's policy writer makes names its class
// (Pneu-Policy) and its process (Pneu-Instance), which it returns: "" from
// a build before Pneu-Instance, which verify accepts only when the control
// socket says the build is that old. pneu update never reads the install
// token.
func ProbeHTTP(ctx context.Context, port int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/", nil)
	if err != nil {
		return "", err
	}
	req.Host = "pneu.localhost:" + strconv.Itoa(port)
	c := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("loopback HTTP: %w", err)
	}
	resp.Body.Close()
	id := resp.Header.Get("Pneu-Instance")
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Pneu-Policy") == "" || (id != "" && !instanceRE.MatchString(id)) {
		return "", fmt.Errorf("loopback HTTP on %d doesn't answer as pneu (HTTP %d)", port, resp.StatusCode)
	}
	return id, nil
}

// --- skew ---

// refreshSkew records, for the running daemon's build and the server's,
// which is older, and tells the daemon to reread it. Best effort: no
// daemon or no server revision, nothing to record.
func (e *Env) refreshSkew() {
	in, err := control.AskStatus(e.Socket)
	if err != nil || !ValidRevision(in.Revision) {
		return
	}
	if e.recordSkew(in.Revision) {
		control.Send(e.Socket, control.UpdateChecked, control.Timeout)
	}
}

// recordSkew records client against the server's revision as the daemon
// knows it; whether it recorded anything.
func (e *Env) recordSkew(client string) bool {
	sit, err := control.AskSituation(e.Socket)
	if err != nil || sit.Mode != control.ModeClient || !ValidRevision(sit.ServerRevision) || sit.ServerRevision == client {
		return false
	}
	rel := e.Relate(client, sit.ServerRevision)
	if err := SaveCache(e.cache(), Pair{Client: client, Server: sit.ServerRevision, Relation: rel}, e.now()); err != nil {
		fmt.Fprintf(e.Err, "pneu update: couldn't record which build is older: %v\n", err)
		return false
	}
	switch rel {
	case RelClientOlder:
		e.say("The server's build (%s) is newer than this machine's (%s).", short(sit.ServerRevision), short(client))
	case RelServerOlder:
		e.say("The server's build (%s) is older than this machine's (%s): update it on the server.", short(sit.ServerRevision), short(client))
	default:
		e.say("The server's build (%s) and this machine's (%s) differ, and git can't say which is older (%s).", short(sit.ServerRevision), short(client), rel)
	}
	return true
}

// Relate is how client's revision stands to server's in the checkout,
// after a fetch: only `git merge-base --is-ancestor` decides it, never a
// timestamp (R16). The server's revision is the server's choice, so
// nothing here may fetch it: every query runs without lazy fetching from a
// partial clone's promisor remotes (U1), and a commit the checkout lacks
// is unknown.
func (e *Env) Relate(client, server string) string {
	if !ValidRevision(client) || !ValidRevision(server) || client == server {
		return RelUnknown
	}
	for _, r := range []string{client, server} {
		if err := e.gitCmd(true, "cat-file", "-e", r+"^{commit}").Run(); err != nil {
			return RelUnknown
		}
	}
	if ok, err := e.ancestor(client, server); err != nil {
		return RelUnknown
	} else if ok {
		return RelClientOlder
	}
	if ok, err := e.ancestor(server, client); err != nil {
		return RelUnknown
	} else if ok {
		return RelServerOlder
	}
	return RelDiverged
}

// --- children ---

// child is a program pneu update starts. It inherits the update lock (as
// fd 3), so the flock outlives this process for as long as the child, or
// anything it started, still runs (U3); and it gets SIGKILL if this
// process dies. The second part is best effort: Linux sends the parent-
// death signal when the thread that started the child exits (Go doesn't
// end its threads in practice; nothing here locks one), and only to the
// direct child, not what it started. The lock is the guarantee.
func (e *Env) child(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	if e.lock != nil {
		cmd.ExtraFiles = []*os.File{e.lock}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// gitEnv is the environment minus anything that would point git at
// another repository than the recorded checkout, or turn lazy fetching
// back on.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_NO_LAZY_FETCH":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// gitCmd is git on the recorded checkout. noLazy, for everything that
// only reads: never fetch a missing object from a promisor remote
// (--no-lazy-fetch and GIT_NO_LAZY_FETCH=1, git 2.44+), so a revision the
// server named is never fetched from anywhere (U1). Otherwise a step that
// changes the checkout or its worktrees after confirmation (fetch,
// worktree, merge, reset): hooks off (core.hooksPath=/dev/null), so no
// hook the repo points at runs.
func (e *Env) gitCmd(noLazy bool, args ...string) *exec.Cmd {
	pre := []string{"-C", e.Source.Dir}
	if noLazy {
		pre = append(pre, "--no-lazy-fetch")
	} else {
		// No hooks; and no automatic gc or maintenance, which git starts
		// detached: it would inherit the lock and hold it past this run.
		// No fsmonitor either: its daemon would outlive the step with
		// the lock (W3).
		pre = append(pre, "-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.fsmonitor=false")
	}
	cmd := e.child(e.Git, append(pre, args...)...)
	cmd.Env = gitEnv()
	if noLazy {
		cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
		// A read runs while this process holds the lock and changes
		// nothing: it needn't hold it, and mustn't hand it to a daemon
		// (an fsmonitor) that outlives it.
		cmd.ExtraFiles = nil
	}
	return cmd
}

func (e *Env) runGit(noLazy bool, args ...string) (string, error) {
	cmd := e.gitCmd(noLazy, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(terminalSafe(errb.String())))
	}
	return out.String(), nil
}

// git is a read (no lazy fetch); gitMut a step that changes the checkout.
func (e *Env) git(args ...string) (string, error)    { return e.runGit(true, args...) }
func (e *Env) gitMut(args ...string) (string, error) { return e.runGit(false, args...) }

// --- files ---

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

var (
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	txRE     = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func validPhase(p string) bool {
	return p == PhaseBuilt || p == PhaseDeploying || p == PhaseRollingBack
}

// loadRecord reads update.json: nil, nil when there is none; an error,
// and no guess, when it's out of shape (U7: by token, every field once).
func (e *Env) loadRecord() (*Record, error) {
	b, err := readPrivate(e.record(), maxRecord)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	err = strictObject(b, map[string]any{
		"phase": &r.Phase, "tx": &r.Tx, "dir": &r.Dir, "oldHead": &r.OldHead, "oldRevision": &r.OldRevision,
		"oldModified": &r.OldModified, "oldLegacy": &r.OldLegacy, "oldSha256": &r.OldSHA256, "newRevision": &r.NewRevision,
		"newSha256": &r.NewSHA256, "started": &r.Started,
	})
	if err == nil && (!validPhase(r.Phase) || !txRE.MatchString(r.Tx) || !ValidRevision(r.OldHead) ||
		(r.OldRevision != "" && !ValidRevision(r.OldRevision)) || !ValidRevision(r.NewRevision) ||
		!sha256RE.MatchString(r.OldSHA256) || !sha256RE.MatchString(r.NewSHA256)) {
		err = errors.New("a field out of shape")
	}
	if err != nil {
		return nil, fmt.Errorf("%s is out of shape (%v): pneu update won't guess what an interrupted update did. Look at it, put things right by hand, then remove it", e.record(), err)
	}
	return &r, nil
}

func (e *Env) saveRecord(r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(e.record(), append(b, '\n'))
}

// removeRecord removes update.json and syncs its directory, checking
// both: a record that might come back after a crash is reported (U4).
func (e *Env) removeRecord() error {
	if err := os.Remove(e.record()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing update.json: %w", err)
	}
	if err := syncDir(e.StateDir); err != nil {
		return fmt.Errorf("syncing %s after removing update.json: %w", e.StateDir, err)
	}
	return nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sumIs(path, want string) bool {
	got, err := fileSHA(path)
	return err == nil && got == want
}

func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// copyFile copies src to dst by temp file and rename, executable, synced.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func short(r string) string {
	if len(r) >= 12 {
		return r[:12]
	}
	if r == "" {
		return "unknown"
	}
	return r
}
