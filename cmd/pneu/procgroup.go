package main

// Process groups for gmi runs that hold the account's lock (Z1). exec's
// own cancellation (Cancel, then WaitDelay's Process.Kill) reaches the
// leader only: a descendant that ignores SIGINT would outlive it, and the
// lock with it would be released while that descendant still works in the
// lieer repository. So each such run gets a group of its own, and it ends
// only once the whole group is gone: SIGINT to the group, a grace, SIGKILL
// to the group, then wait for kill(-pgid, 0) to say ESRCH.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// groupGrace is how long a group gets between SIGINT and SIGKILL, how long
// Wait waits for pipes a descendant holds after the leader exits, and how
// long the group gets to disappear after SIGKILL.
var groupGrace = 5 * time.Second

// group is a started process group.
type group struct {
	cmd    *exec.Cmd
	pgid   int
	exited chan error // the leader's Wait, once
}

// startGroup starts cmd as the leader of a new process group.
func startGroup(cmd *exec.Cmd) (*group, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant holding the output pipe mustn't keep Wait from
	// returning once the leader has exited.
	cmd.WaitDelay = groupGrace
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &group{cmd: cmd, pgid: cmd.Process.Pid, exited: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			err = nil // the leader succeeded; what held its pipe is reaped below
		}
		g.exited <- err
	}()
	return g, nil
}

// run waits for the leader, or stops the group when ctx ends first; either
// way it returns only once the group is gone (or the bound passed, an
// error).
func (g *group) run(ctx context.Context) error {
	var err error
	select {
	case err = <-g.exited:
	case <-ctx.Done():
		err = g.stop()
	}
	if rerr := g.reap(); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// stop interrupts the group, and kills it if the leader hasn't exited
// within the grace; it returns the leader's result. The caller has not
// read g.exited.
func (g *group) stop() error {
	syscall.Kill(-g.pgid, syscall.SIGINT)
	select {
	case err := <-g.exited:
		return err
	case <-time.After(groupGrace):
	}
	syscall.Kill(-g.pgid, syscall.SIGKILL)
	return <-g.exited
}

// reap ends whatever is left of the group once the leader is gone:
// SIGINT, the grace, SIGKILL (ESRCH at any point means it's gone), then
// waits for the group to be empty. Its members are reparented, and their
// new parent reaps them; a pgid with no members can't be signalled.
func (g *group) reap() error {
	if gone(g.pgid) {
		return nil
	}
	syscall.Kill(-g.pgid, syscall.SIGINT)
	if waitGone(g.pgid, groupGrace) {
		return nil
	}
	syscall.Kill(-g.pgid, syscall.SIGKILL)
	if waitGone(g.pgid, groupGrace) {
		return nil
	}
	return fmt.Errorf("gmi's process group %d didn't go away after SIGKILL", g.pgid)
}

func gone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func waitGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if gone(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
