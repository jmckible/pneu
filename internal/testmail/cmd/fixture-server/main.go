// Command fixture-server builds the testmail fixture in a directory and
// writes a pneu config.json for it, so the browser half can be driven by
// hand against real notmuch databases:
//
//	dir=$(mktemp -d)
//	go run ./internal/testmail/cmd/fixture-server -dir "$dir" -port 7318
//	mkdir -m 700 "$dir/run"
//	PATH=$PWD/testdata/fakegmi:$PATH XDG_STATE_HOME=$dir/state XDG_RUNTIME_DIR=$dir/run \
//	  go run ./cmd/pneu -config "$dir/config.json" -listen 127.0.0.1:7318
//
// fakegmi on PATH keeps the server's sync ticker off the network,
// XDG_STATE_HOME keeps it off your real install token, and XDG_RUNTIME_DIR
// off your real control socket (a real `pneu open` would launch it).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmckible/pneu/internal/testmail"
)

// tb satisfies testing.TB for testmail.Setup outside a test. The embedded
// nil interface supplies the unexported method; only what Setup calls is
// implemented.
type tb struct {
	testing.TB
	dir string
}

func (t tb) Helper()                      {}
func (t tb) TempDir() string              { return t.dir }
func (t tb) Skip(args ...any)             { log.Fatal(append([]any{"skip: "}, args...)...) }
func (t tb) Fatalf(f string, args ...any) { log.Fatalf(f, args...) }
func (t tb) Fatal(args ...any)            { log.Fatal(args...) }
func (t tb) Logf(f string, args ...any)   { log.Printf(f, args...) }
func (t tb) Cleanup(func())               {}
func (t tb) Name() string                 { return "fixture-server" }
func (t tb) Errorf(f string, args ...any) { log.Fatalf(f, args...) }
func (t tb) Skipf(f string, args ...any)  { log.Fatalf("skip: "+f, args...) }

func main() {
	log.SetFlags(0)
	dir := flag.String("dir", "", "directory to build the fixture in (must be empty or absent)")
	port := flag.Int("port", 7318, "port for the config (Host is pneu.localhost:<port>)")
	flag.Parse()
	if *dir == "" {
		log.Fatal("fixture-server: -dir is required")
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Fatal(err)
	}
	env := testmail.Setup(tb{dir: abs})

	type account struct {
		Name          string `json:"name"`
		Email         string `json:"email"`
		NotmuchConfig string `json:"notmuchConfig"`
		GmiDir        string `json:"gmiDir"`
	}
	cfg := struct {
		Port     int       `json:"port"`
		Accounts []account `json:"accounts"`
	}{Port: *port}
	for _, a := range env.Accounts {
		cfg.Accounts = append(cfg.Accounts, account{a.Name, a.Email, a.NotmuchConfig, filepath.Join(a.Root, "gmail")})
		fmt.Printf("NOTMUCH_CONFIG=%s  # %s\n", a.NotmuchConfig, a.Name)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(abs, "config.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println(path)
}
