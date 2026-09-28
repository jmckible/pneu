package main

// init, auth and set, and the credentials check every remote call makes
// (remote.py:Remote.__require_auth__ → authorize → __get_credentials__).

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var scopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.labels",
	"https://www.googleapis.com/auth/gmail.modify",
}

// sharedClientID is remote.py:Remote.OAUTH2_CLIENT_SECRET's client, which
// lieer falls back to whenever no -c is given.
const sharedClientID = "753933720722-ju82fu305lii0v9rdo6mf9hj40l5juv0.apps.googleusercontent.com"

// initialize is gmailieer.py:Gmailieer.initialize and
// local.py:Local.initialize_repository.
func (g *gmi) initialize() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Printf("initializing repository in: %s..\n", wd)
	fr := lieerFrame("gmailieer.py", 422, "initialize", "self.local.initialize_repository(args.replace_slash_with_dot, args.account)")
	if _, err := os.Stat(configFile); err == nil {
		return raise("lieer.local.Local.RepositoryException: '.gmailieer.json' exists: this repository seems to already be set up!",
			fr, lieerFrame("local.py", 430, "initialize_repository", "raise Local.RepositoryException("))
	}
	if _, err := os.Stat("mail"); err == nil {
		return raise("lieer.local.Local.RepositoryException: 'mail' exists: this repository seems to already be set up!",
			fr, lieerFrame("local.py", 435, "initialize_repository", "raise Local.RepositoryException("))
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	c.ReplaceSlashWithDot = g.args["replace_slash_with_dot"] != ""
	c.Account = g.pos[0]
	if err := c.write(); err != nil {
		return err
	}
	for _, d := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join("mail", d), 0o777); err != nil {
			return err
		}
	}
	if g.args["no_auth"] != "" {
		return nil
	}
	err = g.loadRepository(false)
	if err == nil {
		err = g.authorize(false)
	}
	if err != nil {
		fmt.Print("\n\ninit: repository is set up, but authorization failed. re-run 'gmi auth' with proper parameters to complete authorization\n\n\n\n\n")
	}
	return err
}

// set is gmailieer.py:Gmailieer.set.
func (g *gmi) set() error {
	if err := g.loadRepository(false); err != nil {
		return err
	}
	c := g.local.cfg
	a := g.args
	on := func(k string) bool { return a[k] != "" }
	list := func(s string) []string {
		if strings.TrimSpace(s) == "" {
			return []string{}
		}
		var out []string
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
		return out
	}
	if v, ok := a["timeout"]; ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return usageErr{fmt.Sprintf("argument -t/--timeout: invalid float value: %s", pyRepr(v))}
		}
		c.Timeout = numLit(pyJSON(f))
	}
	switch {
	case on("replace_slash_with_dot"):
		c.ReplaceSlashWithDot = true
	case on("no_replace_slash_with_dot"):
		c.ReplaceSlashWithDot = false
	}
	switch {
	case on("drop_non_existing_labels"):
		c.DropNonExistingLabel = true
	case on("no_drop_non_existing_labels"):
		c.DropNonExistingLabel = false
	}
	switch {
	case on("ignore_empty_history"):
		c.IgnoreEmptyHistory = true
	case on("no_ignore_empty_history"):
		c.IgnoreEmptyHistory = false
	}
	switch {
	case on("remove_local_messages"):
		c.RemoveLocalMessages = true
	case on("no_remove_local_messages"):
		c.RemoveLocalMessages = false
	}
	if v, ok := a["ignore_tags_local"]; ok {
		c.IgnoreTags = list(v)
	}
	if v, ok := a["ignore_tags_remote"]; ok {
		c.IgnoreRemoteLabels = list(v)
	}
	if v, ok := a["file_extension"]; ok {
		c.FileExtension = strings.TrimSpace(v)
	}
	if v, ok := a["local_trash_tag"]; ok {
		if strings.Contains(v, ",") {
			fmt.Println("The local_trash_tag must be a single tag, not a list.  Commas are not allowed.")
			return raise("ValueError", lieerFrame("local.py", 236, "set_local_trash_tag", "raise ValueError()"))
		}
		c.LocalTrashTag = strings.TrimSpace(v)
		if c.LocalTrashTag == "" {
			c.LocalTrashTag = "trash"
		}
	}
	if v, ok := a["translation_list_overlay"]; ok {
		c.TranslationListOverly = list(v)
		if len(c.TranslationListOverly)%2 != 0 {
			return raise("Exception: Translation list overlay must have an even number of items: "+pyList(c.TranslationListOverly),
				lieerFrame("local.py", 246, "set_translation_list_overlay", "raise Exception("))
		}
	}
	// Each set_* method writes the config; with no option nothing is written.
	for _, canon := range specs["set"] {
		if _, ok := a[strings.TrimPrefix(canon, "=")]; ok {
			if err := c.write(); err != nil {
				return err
			}
			break
		}
	}
	timeout, _ := strconv.ParseFloat(string(c.Timeout), 64)
	st := g.local.st
	fmt.Println("Repository information and settings:")
	fmt.Printf("Account ...........: %s\n", c.Account)
	fmt.Printf("historyId .........: %d\n", st.LastHistoryID)
	fmt.Printf("lastmod ...........: %d\n", st.Lastmod)
	fmt.Printf("Timeout ...........: %f\n", timeout)
	fmt.Printf("File extension ....: %s\n", c.FileExtension)
	fmt.Println("Remove local messages .....:", pyBool(c.RemoveLocalMessages))
	fmt.Println("Drop non existing labels...:", pyBool(c.DropNonExistingLabel))
	fmt.Println("Ignore empty history ......:", pyBool(c.IgnoreEmptyHistory))
	fmt.Println("Replace . with / ..........:", pyBool(c.ReplaceSlashWithDot))
	fmt.Println("Ignore tags (local) .......:", pySet(c.IgnoreTags))
	fmt.Println("Ignore labels (remote) ....:", pySet(c.IgnoreRemoteLabels))
	fmt.Println("Trash tag (local) .........:", c.LocalTrashTag)
	fmt.Println("Translation list overlay ..:", pyList(c.TranslationListOverly))
	return nil
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// credentials is what google.oauth2.credentials.Credentials.to_json writes,
// plus the stub's revocation mark (Google's side of a dead refresh token).
type credentials struct {
	Token        string   `json:"token"`
	RefreshToken string   `json:"refresh_token"`
	TokenURI     string   `json:"token_uri"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Scopes       []string `json:"scopes"`
	Expiry       string   `json:"expiry"`
	StubRevoked  bool     `json:"stub_revoked"`
}

func (c *credentials) write() error {
	return os.WriteFile(credentialsFile, []byte(pyJSON([]kv{
		{"token", c.Token}, {"refresh_token", c.RefreshToken}, {"token_uri", c.TokenURI},
		{"client_id", c.ClientID}, {"client_secret", c.ClientSecret}, {"scopes", c.Scopes},
		{"universe_domain", "googleapis.com"}, {"account", ""}, {"expiry", c.Expiry},
		{"stub_revoked", c.StubRevoked},
	})), 0o666)
}

// getLabels is the remote.get_labels() every pull, push, sync and send
// starts with; its auth check is where a dead token surfaces.
func (g *gmi) getLabels() error {
	if err := g.authorize(false); err != nil {
		return err
	}
	box, err := loadMailbox(cmpOr(os.Getenv("STUBGMI_FIXTURE"), "all"), envInt("STUBGMI_COUNT", 0))
	if err != nil {
		return err
	}
	g.box = box
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// callerFrame is where authorize was reached from, for the traceback.
func (g *gmi) callerFrame() frame {
	switch g.action {
	case "auth":
		return lieerFrame("gmailieer.py", 445, "authorize", "self.remote.authorize(args.force)")
	case "init":
		return lieerFrame("gmailieer.py", 433, "initialize", "self.remote.authorize()")
	case "sync":
		return lieerFrame("gmailieer.py", 498, "sync", "self.remote.get_labels()")
	case "push":
		return lieerFrame("gmailieer.py", 515, "push", "self.remote.get_labels()")
	case "send":
		return lieerFrame("gmailieer.py", 1019, "send", "self.remote.get_labels()")
	}
	return lieerFrame("gmailieer.py", 618, "pull", "self.remote.get_labels()  # to make sure label map is initialized")
}

// authorize is Remote.authorize and __get_credentials__.
func (g *gmi) authorize(reauth bool) error {
	if reauth {
		if _, err := os.Stat(credentialsFile); err == nil {
			fmt.Println("reauthorizing..")
			os.Remove(credentialsFile)
		}
	}
	var viaAuth []frame
	if g.action != "auth" && g.action != "init" {
		viaAuth = append(viaAuth, lieerFrame("remote.py", 138, "func_wrap", "self.authorize()"))
	}
	getCreds := lieerFrame("remote.py", 495, "authorize", "self.credentials = self.__get_credentials__()")
	if b, err := os.ReadFile(credentialsFile); err == nil {
		var c credentials
		if err := json.Unmarshal(b, &c); err != nil || c.RefreshToken == "" {
			return raise("ValueError: Authorized user info was not in the expected format, missing fields refresh_token.",
				append(append([]frame{g.callerFrame()}, viaAuth...), getCreds,
					lieerFrame("remote.py", 525, "__get_credentials__", "credentials = Credentials.from_authorized_user_file("))...)
		}
		if !c.StubRevoked && g.local.st.LastHistoryID > 0 && failMode("token") {
			c.StubRevoked = true
			c.write()
		}
		if c.StubRevoked {
			return refreshError(append(append([]frame{g.callerFrame()}, viaAuth...), getCreds))
		}
		return nil
	}
	clientID, authURI := sharedClientID, "https://accounts.google.com/o/oauth2/auth"
	if cf := g.args["credentials"]; cf != "" {
		fmt.Println("auth: using user-provided api id and secret")
		b, err := os.ReadFile(cf)
		if err != nil {
			return raise("lieer.remote.Remote.GenericException: error: no secret client API key file found for authentication at: "+cf,
				g.callerFrame(), getCreds, lieerFrame("remote.py", 537, "__get_credentials__", "raise Remote.GenericException("))
		}
		var j map[string]struct {
			ClientID string `json:"client_id"`
			AuthURI  string `json:"auth_uri"`
		}
		json.Unmarshal(b, &j)
		inst, ok := j["installed"]
		if !ok {
			inst, ok = j["web"]
		}
		if !ok {
			return raise("ValueError: Client secrets must be for a web or installed app.",
				g.callerFrame(), getCreds, lieerFrame("remote.py", 542, "__get_credentials__", "flow = InstalledAppFlow.from_client_secrets_file("))
		}
		clientID, authURI = inst.ClientID, cmpOr(inst.AuthURI, authURI)
	}
	if err := g.consent(clientID, authURI, append(append([]frame{g.callerFrame()}, viaAuth...), getCreds)); err != nil {
		return err
	}
	return (&credentials{
		Token: "ya29.stub-" + randHex(16), RefreshToken: "1//stub-" + randHex(16), TokenURI: "https://oauth2.googleapis.com/token",
		ClientID: clientID, ClientSecret: "stub-secret", Scopes: scopes,
		Expiry: time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z"),
	}).write()
}

// refreshError is google-auth's invalid_grant failure, with its real frames
// (google/oauth2 in python-google-auth as installed on Arch).
func refreshError(lieer []frame) error {
	const ga = "/usr/lib/python3.14/site-packages/google"
	fr := append(lieer,
		lieerFrame("remote.py", 531, "__get_credentials__", "credentials.refresh(Request())"),
		frame{ga + "/auth/credentials.py", 517, "refresh", "self._perform_refresh_token(request)"},
		frame{ga + "/oauth2/credentials.py", 435, "_perform_refresh_token", ") = reauth.refresh_grant("},
		frame{ga + "/oauth2/reauth.py", 370, "refresh_grant", "_client._handle_error_response(response_data, retryable_error)"},
		frame{ga + "/oauth2/_client.py", 73, "_handle_error_response", "raise exceptions.RefreshError("},
	)
	return raise("google.auth.exceptions.RefreshError: ('invalid_grant: Token has been expired or revoked.', {'error': 'invalid_grant', 'error_description': 'Token has been expired or revoked.'})", fr...)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const consentPrompt = "Please visit this URL to authorize this application: "

// consent is InstalledAppFlow.run_local_server() as lieer calls it: no
// arguments, so localhost:8080, open_browser=True, no timeout.
func (g *gmi) consent(clientID, authURI string, frames []frame) error {
	state := randHex(15)
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"http://localhost:8080/"},
		"scope": {strings.Join(scopes, " ")}, "state": {state}, "code_challenge": {randHex(21)},
		"code_challenge_method": {"S256"}, "access_type": {"offline"},
	}
	flow := lieerFrame("remote.py", 545, "__get_credentials__", "credentials = flow.run_local_server()")
	if g.args["credentials"] == "" {
		flow = lieerFrame("remote.py", 559, "__get_credentials__", "credentials = flow.run_local_server()")
	}
	frames = append(frames, flow)
	const oauth = "/usr/lib/python3.14/site-packages/google_auth_oauthlib/flow.py"
	if os.Getenv("STUBGMI_AUTH") != "browser" {
		fmt.Println(consentPrompt + authURI + "?" + q.Encode())
		if g.action == "auth" || g.action == "init" {
			return nil // instant consent
		}
		fmt.Fprintln(os.Stderr, "stubgmi: this run has no credentials, so real lieer would now wait for the browser consent above; run `gmi auth` first")
		return errNoConsent
	}
	ln, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		return raise("OSError: [Errno 98] Address already in use",
			append(frames, frame{oauth, 439, "run_local_server", "local_server = wsgiref.simple_server.make_server("})...)
	}
	defer ln.Close()
	u := authURI + "?" + q.Encode()
	if err := openBrowser(u); err != nil {
		return raise("webbrowser.Error: could not locate runnable browser",
			append(frames, frame{oauth, 458, "run_local_server", "webbrowser.get(browser).open(auth_url, new=1, autoraise=True)"},
				frame{"/usr/lib/python3.14/webbrowser.py", 68, "get", "raise Error(\"could not locate runnable browser\")"})...)
	}
	// print() of a Python whose stdout is a pipe is block-buffered: without
	// PYTHONUNBUFFERED the prompt surfaces only when lieer exits.
	if pyBuffered() {
		defer fmt.Println(consentPrompt + u)
	} else {
		fmt.Println(consentPrompt + u)
	}
	// handle_request(): exactly one request, whatever it is.
	tl := ln.(*net.TCPListener)
	for {
		if interrupted.Load() {
			return errInterrupt
		}
		tl.SetDeadline(time.Now().Add(100 * time.Millisecond))
		conn, err := tl.Accept()
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		if err != nil {
			return err
		}
		req, rerr := http.ReadRequest(bufio.NewReader(conn))
		conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-type: text/plain; charset=utf-8\r\n\r\nThe authentication flow has completed. You may close this window."))
		conn.Close()
		fetch := frame{oauth, 478, "run_local_server", "self.fetch_token("}
		if rerr != nil || req.URL.Query().Get("state") != state {
			return raise("oauthlib.oauth2.rfc6749.errors.MismatchingStateError: (mismatching_state) CSRF Warning! State not equal in request and response.", append(frames, fetch)...)
		}
		if req.URL.Query().Get("code") == "" {
			return raise("oauthlib.oauth2.rfc6749.errors.MissingCodeError: (missing_code) Missing code parameter in response.", append(frames, fetch)...)
		}
		return nil
	}
}

// pyBuffered reports whether Python would block-buffer stdout: it isn't a
// TTY and PYTHONUNBUFFERED is unset.
func pyBuffered() bool {
	if os.Getenv("PYTHONUNBUFFERED") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}

// openBrowser is webbrowser.get(None).open(url): $BROWSER's first entry, or
// xdg-open when a display is set, else "could not locate runnable browser".
func openBrowser(u string) error {
	cmdline := ""
	if b := os.Getenv("BROWSER"); b != "" {
		cmdline = strings.Split(b, string(os.PathListSeparator))[0]
	} else if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("xdg-open"); err == nil {
			cmdline = "xdg-open"
		}
	}
	if cmdline == "" {
		return errors.New("could not locate runnable browser")
	}
	args := strings.Fields(cmdline)
	if strings.Contains(cmdline, "%s") {
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], "%s", u)
		}
	} else {
		args = append(args, u)
	}
	exec.Command(args[0], args[1:]...).Run()
	return nil
}
