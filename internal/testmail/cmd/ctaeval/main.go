// Command ctaeval is the read-only half of scripts/ctaeval (docs/actions.md,
// "Evaluating it"): it writes the N most recent HTML bodies of one account,
// exactly as the thread view would render them, into a directory for the
// browser half, with each one's sender domain and nothing else.
//
//	ctaeval -config ~/.config/pneu/config.json -account personal -n 300 -out DIR
//
// It only reads: `notmuch search` and `notmuch show` with the account's
// NOTMUCH_CONFIG, as the server runs them. It never tags, never runs gmi,
// never opens the maildir itself. DIR must not exist; it is created 0700,
// the files in it 0600. Removing it is the caller's job (the script does,
// on exit and on interrupt).
//
// Its errors are categories (ErrSearch, ErrShow, ...), never the error
// underneath: notmuch's stderr and paths can name a message.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/mail"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/web"
)

// Query is what is evaluated: mail with an HTML part, received rather than
// written. Parts that turn out not to be the rendered body (an attached
// .html) are skipped by the server's own choice, web.HTMLBody.
const Query = "mimetype:text/html and not tag:sent and not tag:draft"

// The errors Extract returns, and all main prints of one.
var (
	ErrOut    = errors.New("cannot create the output directory")
	ErrSearch = errors.New("notmuch search failed")
	ErrShow   = errors.New("notmuch show failed")
	ErrWrite  = errors.New("cannot write a body")
)

// Entry is one extracted body: its file in DIR and its sender's domain.
type Entry struct {
	File   string `json:"file"`
	Domain string `json:"domain"`
}

func main() {
	log.SetFlags(0)
	def, _ := config.DefaultPath()
	cfgPath := flag.String("config", def, "pneu config")
	name := flag.String("account", "", "account name")
	n := flag.Int("n", 300, "how many HTML bodies")
	out := flag.String("out", "", "directory to create for the bodies")
	flag.Parse()
	if *name == "" || *out == "" || *n <= 0 {
		log.Fatal("usage: ctaeval -account NAME -out DIR [-n N] [-config PATH]")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal("ctaeval: cannot read the pneu config")
	}
	a, ok := cfg.Account(*name)
	if !ok {
		log.Fatal("ctaeval: no such account in the pneu config")
	}
	acct := notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig}
	// A signal cancels the context every notmuch run is started with, so
	// the run is killed and reaped rather than left behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	got, err := Extract(ctx, acct, *n, *out)
	if ctx.Err() != nil {
		stop()
		fmt.Fprintln(os.Stderr, "ctaeval: interrupted")
		os.Exit(130)
	}
	if err != nil {
		log.Fatal("ctaeval: " + err.Error())
	}
	fmt.Fprintf(os.Stderr, "ctaeval: %d HTML bodies from %s\n", len(got), a.Name)
}

// Extract writes up to n HTML bodies of acct, newest first, into dir
// (created 0700; it must not exist) as 0000.html, 0001.html, ..., and
// manifest.json, the []Entry in that order ([] when there are none). Its
// errors are the categories above.
func Extract(ctx context.Context, acct notmuch.Account, n int, dir string) ([]Entry, error) {
	return extract(ctx, acct, Query, n, dir)
}

func extract(ctx context.Context, acct notmuch.Account, query string, n int, dir string) ([]Entry, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, ErrOut
	}
	got := []Entry{}
	page := max(2*n, 100)
	for off := 0; len(got) < n; off += page {
		ids, err := acct.MessageIDsPage(ctx, query, notmuch.SearchOpts{Limit: page, Offset: off})
		if err != nil {
			return nil, ErrSearch
		}
		for _, id := range ids {
			if len(got) == n {
				break
			}
			m, err := acct.Message(ctx, id)
			if errors.Is(err, notmuch.ErrNotFound) {
				continue // gone since the search
			}
			if err != nil {
				return nil, ErrShow
			}
			html, ok := web.HTMLBody(&m)
			if !ok {
				continue
			}
			e := Entry{File: fmt.Sprintf("%04d.html", len(got)), Domain: senderDomain(m.Headers["From"])}
			if err := writeFile(filepath.Join(dir, e.File), []byte(html)); err != nil {
				return nil, ErrWrite
			}
			got = append(got, e)
		}
		if len(ids) < page {
			break
		}
	}
	b, err := json.Marshal(got)
	if err != nil {
		return nil, ErrWrite
	}
	if err := writeFile(filepath.Join(dir, "manifest.json"), b); err != nil {
		return nil, ErrWrite
	}
	return got, nil
}

func writeFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// senderDomain is From's domain, lowercased, or "" when it has none.
func senderDomain(from string) string {
	addr := from
	if p, err := mail.ParseAddress(from); err == nil {
		addr = p.Address
	} else if i, j := strings.LastIndex(from, "<"), strings.LastIndex(from, ">"); i >= 0 && j > i {
		addr = from[i+1 : j]
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return ""
	}
	d := strings.ToLower(strings.Trim(strings.TrimSpace(addr[at+1:]), ">. "))
	if strings.ContainsAny(d, " \t\"<>@") {
		return ""
	}
	return d
}
