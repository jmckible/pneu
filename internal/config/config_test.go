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

func TestPeerAndServer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	acct := `"accounts":[{"name":"a","email":"a@x","notmuchConfig":"/n","gmiDir":"/g"}]`
	cases := map[string]string{
		`{` + acct + `}`:                                                         "",
		`{` + acct + `,"peer":{"port":7320}}`:                                    "",
		`{` + acct + `,"peer":{"port":0}}`:                                       "bad peer port",
		`{` + acct + `,"peer":{"port":70000}}`:                                   "bad peer port",
		`{` + acct + `,"peer":{"port":7317}}`:                                    "bad peer port",
		`{` + acct + `,"server":{"ssh":"dell"}}`:                                 "both accounts and server",
		`{"server":{"ssh":"dell","node":"n1","port":7320}}`:                      "",
		`{"server":{"ssh":"dell","node":"n1","port":7320},"peer":{"port":7321}}`: "both peer and server",
		`{"server":{"ssh":"-oProxyCommand=x","node":"n1","port":7320}}`:          "bad ssh target",
		`{"server":{"ssh":"a b","node":"n1","port":7320}}`:                       "bad ssh target",
		`{"server":{"ssh":"dell","node":"n-1","port":7320}}`:                     "bad node",
		`{"server":{"ssh":"dell","node":"n1","port":0}}`:                         "bad port",
	}
	for body, want := range cases {
		os.WriteFile(p, []byte(body), 0o600)
		c, err := Load(p)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", body, err, want)
		case want == "" && strings.Contains(body, "server") && (c.Server == nil || c.Server.Node != "n1" || c.Server.Port != 7320 || len(c.Accounts) != 0):
			t.Errorf("%s: server %+v", body, c.Server)
		case want == "" && strings.Contains(body, "peer") && (c.Peer == nil || c.Peer.Port != 7320):
			t.Errorf("%s: peer %+v", body, c.Peer)
		case want == "" && !strings.Contains(body, "peer") && c.Peer != nil:
			t.Errorf("%s: peer %+v without a block", body, c.Peer)
		}
	}
	// ReadRaw and Write keep both blocks as written.
	os.WriteFile(p, []byte(`{"port":7317,"peer":{"port":7320},"server":{"ssh":"dell"}}`), 0o600)
	raw, err := ReadRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(p, raw); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"ssh": "dell"`) || !strings.Contains(string(b), `"port": 7320`) {
		t.Fatalf("round trip lost a block: %s", b)
	}
}
