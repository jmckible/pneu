package gmi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultLockPath is the flock every gmi run for the lieer dir gmiDir
// serializes on: .gmi.lock beside it, outside lieer's repository.
func DefaultLockPath(gmiDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(gmiDir)), ".gmi.lock")
}

// LockForExec takes the same exclusive flock for a process about to exec
// gmi (`pneu gmi`). The fd is opened without close-on-exec, so the exec'd
// gmi holds the lock for its whole life and the kernel drops it when gmi
// exits, however it exits. It waits up to wait, calling onWait once if the
// lock is held. On timeout the waiting flock is abandoned, as in flockFile.
func LockForExec(path string, wait time.Duration, onWait func()) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT, 0o644) // no O_CLOEXEC, unlike os.OpenFile
	if err != nil {
		return -1, fmt.Errorf("gmi lock %s: %w", path, err)
	}
	err = retryEINTR(func() error { return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) })
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		syscall.Close(fd)
		return -1, fmt.Errorf("gmi lock %s: %w", path, err)
	}
	if onWait != nil {
		onWait()
	}
	done := make(chan error, 1)
	go func() { done <- retryEINTR(func() error { return syscall.Flock(fd, syscall.LOCK_EX) }) }()
	select {
	case err := <-done:
		if err != nil {
			syscall.Close(fd)
			return -1, fmt.Errorf("gmi lock %s: %w", path, err)
		}
		return fd, nil
	case <-time.After(wait):
		go func() {
			<-done
			syscall.Close(fd)
		}()
		return -1, fmt.Errorf("gmi lock %s: still held after %v (a sync is running; see journalctl --user -u pneu)", path, wait)
	}
}

// flockFile takes an exclusive flock(2) on path, the same lock `pneu gmi`
// waits on for manual runs. It tries
// non-blocking first; if the lock is held it reports contention via onWait and
// blocks until the lock is free or ctx ends. The returned func releases it.
//
// A blocking flock can't be interrupted, so on ctx cancel the waiting
// goroutine is abandoned: it closes its fd (dropping the lock) as soon as the
// flock returns.
func flockFile(ctx context.Context, path string, onWait func()) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("gmi lock: %w", err)
	}
	fd := int(f.Fd())
	release = func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}

	err = retryEINTR(func() error { return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) })
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, fmt.Errorf("gmi lock %s: %w", path, err)
	}
	if onWait != nil {
		onWait()
	}

	done := make(chan error, 1)
	go func() {
		done <- retryEINTR(func() error { return syscall.Flock(fd, syscall.LOCK_EX) })
	}()
	select {
	case err := <-done:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("gmi lock %s: %w", path, err)
		}
		return release, nil
	case <-ctx.Done():
		go func() {
			<-done
			f.Close()
		}()
		return nil, ctx.Err()
	}
}

func retryEINTR(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// Lock takes path's flock for a run of gmi as a child (`pneu account`),
// waiting up to wait and calling onWait once if it is held. The returned
// func releases it.
func Lock(path string, wait time.Duration, onWait func()) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	release, err := flockFile(ctx, path, onWait)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("gmi lock %s: still held after %v (pneu is syncing; see journalctl --user -u pneu)", path, wait)
	}
	return release, err
}
