package gmi

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// `pneu gmi` and the engine must exclude each other on the same file, and
// the manual side's fd must survive exec.
func TestLockForExec(t *testing.T) {
	path := DefaultLockPath(filepath.Join(t.TempDir(), "personal", "gmail"))
	if filepath.Base(path) != ".gmi.lock" || filepath.Base(filepath.Dir(path)) != "personal" {
		t.Fatalf("DefaultLockPath = %s", path)
	}
	syscall.Mkdir(filepath.Dir(path), 0o755)

	release, err := flockFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := 0
	if _, err := LockForExec(path, 50*time.Millisecond, func() { waited++ }); err == nil || waited != 1 {
		t.Fatalf("lock held by the engine: err=%v, onWait calls=%d", err, waited)
	}
	release()

	fd, err := LockForExec(path, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
		t.Fatalf("fd flags %#x (errno %v): close-on-exec would drop the lock at exec", flags, errno)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := flockFile(ctx, path, nil); err == nil {
		t.Fatal("engine took the lock while pneu gmi held it")
	}
}
