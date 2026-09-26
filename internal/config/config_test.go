package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("HOME", "/h")
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "INSTALL.md") {
		t.Fatalf("missing config: %v, want an error naming the path and INSTALL.md", err)
	}

	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"accounts":[{"name":"a","email":"a@x","notmuchConfig":"~/nm","gmiDir":"/abs"}]}`), 0o600)
	c, err := Load(p)
	if err != nil || c.Port != 7317 || c.Accounts[0].NotmuchConfig != "/h/nm" || c.Accounts[0].GmiDir != "/abs" {
		t.Fatalf("file: %+v %v", c, err)
	}
	if a, ok := c.Account("a"); !ok || a.GmiDir != "/abs" {
		t.Fatalf("Account(a) = %+v %v", a, ok)
	}
	if _, ok := c.Account("b"); ok {
		t.Fatal("Account(b) found an unconfigured account")
	}

	os.WriteFile(p, []byte(`{"accounts":[{"name":"a"},{"name":"a"}]}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("duplicate names should error")
	}
}
