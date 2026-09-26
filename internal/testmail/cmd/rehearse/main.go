// Command rehearse is scripts/rehearse's Go half.
//
//	rehearse script INSTALL.md          INSTALL.md's steps as a bash script
//	rehearse script --uninstall INSTALL.md
//	                                    its Uninstall section, the same way
//	rehearse bridge-in  SOCK ADDR       inside the sandbox's network namespace:
//	                                    relay connections on unix socket SOCK to ADDR
//	rehearse grant CONSENT_URL          what Google does when the person allows
//	                                    access: GET its redirect_uri with its
//	                                    state and a code (stubgmi's consent)
//	rehearse bridge-out LISTEN SOCK HOST
//	                                    outside it: serve LISTEN, rewrite Host and
//	                                    Origin to HOST, relay to SOCK
//
// The sandbox has its own loopback, so its pneu listens on the port
// INSTALL.md names even while the real one runs. A browser reaches it
// through the bridge at another name and port (rehearse.localhost:7417):
// a different host keeps the sandbox's session cookie away from the real
// pneu's, which shares the cookie name and would be overwritten otherwise.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("rehearse: ")
	if len(os.Args) < 2 {
		log.Fatal("usage: rehearse script [--uninstall] INSTALL.md | bridge-in SOCK ADDR | bridge-out LISTEN SOCK HOST")
	}
	var err error
	switch a := os.Args[2:]; os.Args[1] {
	case "script":
		uninstall := len(a) == 2 && a[0] == "--uninstall"
		if uninstall {
			a = a[1:]
		}
		if len(a) != 1 {
			log.Fatal("usage: rehearse script [--uninstall] INSTALL.md")
		}
		err = script(a[0], uninstall, os.Stdout)
	case "grant":
		if len(a) != 1 {
			log.Fatal("usage: rehearse grant CONSENT_URL")
		}
		err = grant(a[0])
	case "bridge-in":
		if len(a) != 2 {
			log.Fatal("usage: rehearse bridge-in SOCK ADDR")
		}
		err = bridgeIn(a[0], a[1])
	case "bridge-out":
		if len(a) != 3 {
			log.Fatal("usage: rehearse bridge-out LISTEN SOCK HOST")
		}
		err = bridgeOut(a[0], a[1], a[2])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

var (
	fence       = regexp.MustCompile("^```(\\w*)\\s*$")
	pathLead    = regexp.MustCompile("^`(~?/[^`]+)`[^`]*:\\s*$")
	placeholder = regexp.MustCompile(`<[A-Za-z][A-Za-z ]*>`)
)

// substitute fills INSTALL.md's placeholders from the rehearsal's shell
// variables. Anything it doesn't know is an error: INSTALL.md grew a
// placeholder the rehearsal can't fill.
func substitute(line string, lineNo int) (string, error) {
	line = strings.ReplaceAll(line, "/home/<you>", "$HOME")
	repl := map[string]string{
		"<acct>": "$ACCT", "<address>": "$ADDRESS", "<you>": "$USER",
		"<Your Name>": "$FULLNAME", "<other address>": "$OTHER_ADDRESS", "<client JSON>": "$CLIENT_JSON",
	}
	var bad error
	out := placeholder.ReplaceAllStringFunc(line, func(p string) string {
		if v, ok := repl[p]; ok {
			return v
		}
		bad = fmt.Errorf("INSTALL.md:%d: placeholder %s has no rehearsal value", lineNo, p)
		return p
	})
	return out, bad
}

// script turns the numbered steps of INSTALL.md (from "## 1." up to
// "## Uninstall"), or with uninstall the Uninstall section, into bash that
// calls the functions scripts/rehearse defines:
//
//	step TITLE            a "## " heading
//	human TITLE           a heading or paragraph marked (human)
//	cmd LINE              one line of a ```sh block, echoed then run
//	write_file PATH       a non-shell block after a line that starts with
//	                      `PATH` and ends with a colon
func script(path string, uninstall bool, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	on := false
	lang, inBlock := "", false
	var block []string
	blockStart, lineNo := 0, 0
	lastText, target := "", ""
	fmt.Fprintln(w, "# generated from", path, "by rehearse script")
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if !inBlock && strings.HasPrefix(line, "## ") {
			title := strings.TrimPrefix(line, "## ")
			if uninstall {
				on = title == "Uninstall"
			} else if strings.HasPrefix(title, "1.") {
				on = true
			} else if title == "Uninstall" {
				on = false
			}
			if on {
				fmt.Fprintf(w, "step %s\n", shq(title))
				if strings.Contains(title, "(human)") {
					fmt.Fprintf(w, "human %s\n", shq(title))
				}
			}
			continue
		}
		if !on {
			continue
		}
		if m := fence.FindStringSubmatch(line); m != nil {
			if !inBlock {
				inBlock, lang, block, blockStart = true, m[1], nil, lineNo
				target = ""
				if pm := pathLead.FindStringSubmatch(lastText); pm != nil {
					target = pm[1]
				}
				continue
			}
			inBlock = false
			if err := emitBlock(w, lang, target, block, blockStart); err != nil {
				return err
			}
			continue
		}
		if inBlock {
			block = append(block, line)
			continue
		}
		if strings.TrimSpace(line) != "" {
			lastText = line
			if strings.Contains(line, "**(human)**") {
				fmt.Fprintf(w, "human %s\n", shq(strings.TrimSpace(line)))
			}
		}
	}
	return sc.Err()
}

func emitBlock(w io.Writer, lang, target string, block []string, start int) error {
	switch {
	case lang == "sh" || lang == "bash":
		for i, l := range block {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			s, err := substitute(l, start+1+i)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "cmd %s\n", shq(s))
		}
	case target != "":
		p, err := substitute(target, start-1)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "write_file %s <<REHEARSE_EOF\n", strings.Replace(p, "~/", "$HOME/", 1))
		for i, l := range block {
			s, err := substitute(l, start+1+i)
			if err != nil {
				return err
			}
			// The heredoc expands $VARS; escape anything else bash would touch.
			s = strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(s)
			s = regexp.MustCompile(`\$([^A-Z{]|$)`).ReplaceAllString(s, `\$$$1`)
			fmt.Fprintln(w, s)
		}
		fmt.Fprintln(w, "REHEARSE_EOF")
	default:
		fmt.Fprintf(w, "note %s\n", shq(fmt.Sprintf("INSTALL.md:%d: a ```%s block with no target path; not run", start, lang)))
	}
	return nil
}

// shq single-quotes s for bash.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func grant(consent string) error {
	u, err := url.Parse(consent)
	if err != nil {
		return err
	}
	q := u.Query()
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://localhost:") {
		return fmt.Errorf("consent URL has no loopback redirect_uri: %q", consent)
	}
	resp, err := http.Get(redirect + "?" + url.Values{"state": {q.Get("state")}, "code": {"4/rehearsal-code"}}.Encode())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func bridgeIn(sock, addr string) error {
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			up, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			defer up.Close()
			go io.Copy(up, c)
			io.Copy(c, up)
		}()
	}
}

func bridgeOut(listen, sock, host string) error {
	public := "http://" + listen
	if h, port, err := net.SplitHostPort(listen); err == nil && (h == "127.0.0.1" || h == "localhost") {
		public = "http://rehearse.localhost:" + port
	}
	pub, _ := url.Parse(public)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", host
			pr.Out.Host = host
			if pr.In.Header.Get("Origin") == public {
				pr.Out.Header.Set("Origin", "http://"+host)
			}
			if ref := pr.In.Header.Get("Referer"); strings.HasPrefix(ref, public+"/") {
				pr.Out.Header.Set("Referer", "http://"+host+strings.TrimPrefix(ref, public))
			}
		},
		ModifyResponse: func(r *http.Response) error {
			if loc := r.Header.Get("Location"); strings.HasPrefix(loc, "http://"+host) {
				r.Header.Set("Location", public+strings.TrimPrefix(loc, "http://"+host))
			}
			return nil
		},
		Transport: &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", sock) }},
		// SSE: flush every write.
		FlushInterval: -1,
	}
	// Only the rehearsal's own name gets through: the sandbox keeps the
	// real server's Host check meaningful behind the bridge.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != pub.Host {
			http.Error(w, "rehearsal bridge: wrong Host", http.StatusMisdirectedRequest)
			return
		}
		rp.ServeHTTP(w, r)
	})
	return http.ListenAndServe(listen, h)
}
