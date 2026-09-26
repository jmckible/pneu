#!/usr/bin/env bash
# Hostile-corpus check for the mail frame assembly (AGENTS.md: any change to
# the sanitizer, sandbox flags, or CSP is verified here before merge).
#
#   go test ./testdata/hostile        # `go test ./...` skips testdata/
#   testdata/hostile/run.sh           # directly; CHROMIUM=/path overrides
#
# Serves the repo root on a throwaway 127.0.0.1 port (the harness origin, which
# plays the app origin), plus an https server mapped to remote.test (the
# click-to-load image host: not an IP literal, not localhost, and https, which
# is all the relaxed img-src allows). Loads harness.html in headless Chromium
# (it renders every case in cases.json through web/static/mailframe.js, the
# file the app ships), and fails if
#   - any case's checks fail (see harness.html), or top.__pwned is set,
#   - the net log mentions tracker.invalid or a receipt-tracker host from
#     case 38 at all (a blocked request still leaked), or the harness
#     produced no results,
#   - the page requested any host:port other than the two servers (loopback
#     and LAN cases, 36–37),
#   - the harness origin served anything but the harness's own files,
#     /web/static/ and /part/ (app-path cases, 30 and 35: /status, /events),
#     or the https server anything but the pixel.
#
# RUN_SH_KEEP=1 keeps the temp dir (net.json, dom.html, http.log, chromium.log).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
chromium=${CHROMIUM:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)}
[[ -x $chromium ]] || { echo "run.sh: chromium not found" >&2; exit 2; }

command -v openssl >/dev/null || { echo "run.sh: openssl not found" >&2; exit 2; }

tmp=$(mktemp -d)
servers=()
cleanup() {
  for s in "${servers[@]}"; do kill "$s" 2>/dev/null || true; done
  rm -rf "$tmp/profile"
  if [[ -n ${RUN_SH_KEEP:-} ]]; then echo "run.sh: kept $tmp" >&2; else rm -rf "$tmp"; fi
}
trap cleanup EXIT

# The served tree is the repo root plus /part/, because the frame's CSP only
# allows images under the app's part endpoint path: /part/harness-pixel.svg
# for the cid cases, and a message-shaped /part/personal/m@hostile.test/ for
# the narrowed partBase case.
mkdir -p "$tmp/site/part/personal/m@hostile.test"
ln -s "$root/testdata" "$tmp/site/testdata"
ln -s "$root/web" "$tmp/site/web"
ln -s "$here/harness-pixel.svg" "$tmp/site/part/harness-pixel.svg"
ln -s "$here/harness-pixel.svg" "$tmp/site/part/personal/m@hostile.test/harness-pixel.svg"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=remote.test \
  -addext subjectAltName=DNS:remote.test \
  -keyout "$tmp/key.pem" -out "$tmp/cert.pem" >/dev/null 2>&1

# Port 0: each server picks a free port and prints it.
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$tmp/site" >"$tmp/http.log" 2>&1 &
servers+=($!)
python3 -u - "$tmp/site" "$tmp/cert.pem" "$tmp/key.pem" >"$tmp/https.log" 2>&1 <<'PY' &
import functools, http.server, ssl, sys
site, cert, key = sys.argv[1:4]
srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0),
    functools.partial(http.server.SimpleHTTPRequestHandler, directory=site))
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print(f"Serving HTTPS on 127.0.0.1 port {srv.server_address[1]} (https)", flush=True)
srv.serve_forever()
PY
servers+=($!)

port_of() {
  local p=
  for _ in $(seq 100); do
    p=$(sed -n 's/.* port \([0-9][0-9]*\) .*/\1/p' "$1" | head -n1)
    [[ -n $p ]] && break
    sleep 0.05
  done
  [[ -n $p ]] || { echo "run.sh: server did not start" >&2; cat "$1" >&2; exit 2; }
  echo "$p"
}
port=$(port_of "$tmp/http.log")
sport=$(port_of "$tmp/https.log")

url="http://127.0.0.1:$port/testdata/hostile/harness.html?https=$sport"
# Every other name resolves to nothing: case 38 names real receipt-tracker
# hosts, and a leak must show up in the net log without reaching them.
"$chromium" --headless=new --disable-gpu --no-sandbox \
  --user-data-dir="$tmp/profile" --no-first-run --disable-background-networking \
  --disable-component-update --disable-sync --disable-default-apps \
  --host-resolver-rules="MAP remote.test 127.0.0.1, MAP * ~NOTFOUND, EXCLUDE 127.0.0.1" --ignore-certificate-errors \
  --disable-popup-blocking \
  --virtual-time-budget=8000 --log-net-log="$tmp/net.json" \
  --dump-dom "$url" >"$tmp/dom.html" 2>"$tmp/chromium.log" || {
  echo "run.sh: chromium failed" >&2; tail -n 20 "$tmp/chromium.log" >&2; exit 1; }

python3 - "$tmp/dom.html" "$tmp/net.json" "$tmp/http.log" "$tmp/https.log" "$here/cases.json" "$port" "$sport" <<'PY'
import html, json, re, sys
from urllib.parse import urlsplit

dom = open(sys.argv[1], encoding="utf-8").read()
net = open(sys.argv[2], encoding="utf-8", errors="replace").read()
http_log = open(sys.argv[3], encoding="utf-8", errors="replace").read()
https_log = open(sys.argv[4], encoding="utf-8", errors="replace").read()
cases = json.load(open(sys.argv[5], encoding="utf-8"))
port, sport = sys.argv[6], sys.argv[7]
ok = True

m = re.search(r'<pre id="results">(.*?)</pre>', dom, re.S)
try:
    res = json.loads(html.unescape(m.group(1)))
except Exception as e:
    print("FAIL no harness results:", e, (m.group(1)[:200] if m else "no #results"))
    sys.exit(1)
if "error" in res:
    print("FAIL harness error:", res["error"])
    sys.exit(1)

print(f"{'case':<32} {'result':<6} {'canary':<6} {'height':>7} {'csp':>4}  notes")
for c in res["cases"]:
    print(f"{c['id']:<32} {'pass' if c['pass'] else 'FAIL':<6} {str(c.get('canary', '-')).lower():<6} "
          f"{c.get('height', 0):>7} {len(c.get('blocked', [])):>4}  {'; '.join(c['fails'])}")
if res["pwned"] is not None:
    print("FAIL top.__pwned =", res["pwned"])
if res["appScrollsHorizontally"]:
    print("FAIL the harness page scrolls horizontally")
ok = res["pass"]

# Raw text, not parsed events: a DNS lookup, preconnect or blocked request
# that names the host anywhere is a leak.
if not net.strip():
    print("FAIL empty net log")
    ok = False
# Case 38's receipt-tracker hosts are real names, so they are held to the
# same rule (and resolve to nothing, see --host-resolver-rules).
leaky = r"tracker\.invalid|mltrk\.io|superhuman\.com|bl-1\.com|yesware\.com|mixmax\.com|gmtrack\.net|sidekickopen\d*\.com|strk\d*\.email"
hits = sorted(set(re.findall(r"[\w./:-]*(?:" + leaky + r")[\w./?=&%-]*", net, re.I)))
if hits:
    ok = False
    print("FAIL net log mentions tracker.invalid or a receipt-tracker host:")
    for h in hits:
        print("   ", h)

# Report every URL request to a host other than 127.0.0.1. Chromium's own
# background services (component updater, account check) appear even with
# --disable-background-networking; they are listed apart, not failed.
try:
    log = json.loads(net)
    start = log["constants"]["logEventTypes"]["URL_REQUEST_START_JOB"]
    urls = {e["params"]["url"] for e in log["events"]
            if e["type"] == start and "url" in e.get("params", {})}
except Exception:
    urls = set(re.findall(r'"url":"([^"]+)"', net))
browser = re.compile(r"(^|\.)(google\.com|googleapis\.com|gvt1\.com|gstatic\.com)$")
expected = {f"127.0.0.1:{port}", f"remote.test:{sport}"}
page, background = set(), set()
for u in urls:
    parts = urlsplit(u)
    if parts.scheme not in ("http", "https"):
        continue
    host = parts.hostname or ""
    if parts.netloc in expected:
        continue
    (background if browser.search(host) else page).add(parts.netloc)
if page:
    ok = False
    print("FAIL requests to hosts other than the harness servers:", ", ".join(sorted(page)))
if background:
    print("note: chromium background requests:", ", ".join(sorted(background)))

# What the servers actually answered. The harness origin plays the app
# origin, so anything outside the harness's own files, /web/static/ and
# /part/ is a message reaching an app endpoint (30, 35).
def requested(log):
    return [m.group(1) for m in re.finditer(r'"[A-Z]+ (\S+) HTTP/[\d.]+"', log)]
harness = {"/testdata/hostile/" + f for f in
           ("harness.html", "cases.json", "harness-pixel.svg", "opener.html")}
harness |= {"/testdata/hostile/" + c["file"] for c in cases if "file" in c}
harness.add("/favicon.ico")
stray = [p for p in requested(http_log)
         if p.split("?")[0] not in harness and not p.startswith(("/web/static/", "/part/"))]
stray += ["https " + p for p in requested(https_log) if p != "/testdata/hostile/harness-pixel.svg"]
if stray:
    ok = False
    print("FAIL unexpected requests to the harness servers:")
    for p in stray:
        print("   ", p)
if not requested(http_log):
    ok = False
    print("FAIL empty http.log")

total = len(res["cases"])
passed = sum(c["pass"] for c in res["cases"])
print(f"{'PASS' if ok else 'FAIL'}: {passed}/{total} cases, leak hits: {len(hits)}, stray requests: {len(stray)}")
sys.exit(0 if ok else 1)
PY
