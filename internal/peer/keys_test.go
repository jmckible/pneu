package peer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "peer")
	a, err := LoadOrCreateServer(dir, "server")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(dir)
	kf, _ := os.Stat(filepath.Join(dir, keyFile))
	if fi.Mode().Perm() != 0o700 || kf.Mode().Perm() != 0o600 {
		t.Fatalf("modes %v %v", fi.Mode(), kf.Mode())
	}
	b, err := LoadOrCreateServer(dir, "server")
	if err != nil || b.SPKI != a.SPKI || b.PEM != a.PEM {
		t.Fatalf("second load made a new key: %v", err)
	}
	c, err := ParseCertPEM(a.PEM)
	if err != nil || SPKI(c) != a.SPKI {
		t.Fatalf("own PEM: %v", err)
	}
	if strings.Contains(a.PEM, "PRIVATE") {
		t.Fatal("the certificate PEM carries the key")
	}

	// Checked on every load: the file's mode, the directory's.
	os.Chmod(filepath.Join(dir, keyFile), 0o644)
	if _, err := LoadOrCreateServer(dir, "server"); err == nil {
		t.Fatal("0644 key loaded")
	}
	os.Chmod(filepath.Join(dir, keyFile), 0o600)
	os.Chmod(dir, 0o750)
	if _, err := LoadOrCreateServer(dir, "server"); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("0750 dir: %v", err)
	}
	os.Chmod(dir, 0o700|fs.ModeSticky)
	if _, err := LoadOrCreateServer(dir, "server"); err == nil {
		t.Fatal("sticky dir accepted")
	}
	os.Chmod(dir, 0o700)

	// A symlinked directory or key file is refused.
	link := filepath.Join(filepath.Dir(dir), "peerlink")
	os.Symlink(dir, link)
	if _, err := LoadOrCreateServer(link, "server"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlinked dir: %v", err)
	}
	d2 := filepath.Join(t.TempDir(), "peer")
	PrepareDir(d2)
	os.Symlink(filepath.Join(dir, keyFile), filepath.Join(d2, keyFile))
	if _, err := LoadOrCreateServer(d2, "server"); err == nil {
		t.Fatal("symlinked key file loaded")
	}
}

func selfSigned(t *testing.T, key any, pub any) string {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func addJSON(t *testing.T, fields map[string]any) string {
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseAddRequest(t *testing.T) {
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	good := selfSigned(t, p256, &p256.PublicKey)
	other := selfSigned(t, p256, &p256.PublicKey)
	base := map[string]any{"name": "macbook", "node": "nV6M3oJKo721CNTRL", "origin": "http://pneu.localhost:7317", "cert": good}
	with := func(k string, v any) string {
		m := map[string]any{}
		for kk, vv := range base {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return addJSON(t, m)
	}

	req, r, err := ParseAddRequest(strings.NewReader(with("name", "macbook")))
	c, _ := ParseCertPEM(good)
	if err != nil || req.Name != "macbook" || r.SPKI != SPKI(c) || r.Node != "nV6M3oJKo721CNTRL" || r.Origin != "http://pneu.localhost:7317" {
		t.Fatalf("good: %+v %+v %v", req, r, err)
	}

	keyPEM := func() string {
		kb, _ := x509.MarshalPKCS8PrivateKey(p256)
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	}()
	headered := strings.Replace(good, "-----BEGIN CERTIFICATE-----\n", "-----BEGIN CERTIFICATE-----\nProc-Type: 4,ENCRYPTED\n\n", 1)
	bad := map[string]string{
		"oversize":        with("name", strings.Repeat("a", MaxAddRequest)),
		"not json":        "name=macbook",
		"array":           "[" + with("name", "macbook") + "]",
		"two objects":     with("name", "macbook") + with("name", "macbook"),
		"trailing":        with("name", "macbook") + " x",
		"unknown field":   with("extra", "1"),
		"duplicate key":   strings.Replace(with("name", "macbook"), `"name":"macbook"`, `"name":"macbook","name":"air"`, 1),
		"missing name":    with("name", nil),
		"missing cert":    with("cert", nil),
		"number name":     with("name", 7),
		"bad name":        with("name", "Mac Book"),
		"long name":       with("name", strings.Repeat("a", 33)),
		"bad node":        with("node", "n1;rm -rf"),
		"https origin":    with("origin", "https://pneu.localhost:7317"),
		"other host":      with("origin", "http://evil.example:7317"),
		"origin path":     with("origin", "http://pneu.localhost:7317/x"),
		"no port":         with("origin", "http://pneu.localhost"),
		"bad pem":         with("cert", "not a certificate"),
		"two certs":       with("cert", good+other),
		"cert then key":   with("cert", good+keyPEM),
		"key only":        with("cert", keyPEM),
		"leading junk":    with("cert", "junk\n"+good),
		"pem headers":     with("cert", headered),
		"bad block first": with("cert", "-----BEGIN CERTIFICATE-----\nnot-base64\n"+good),
		"unended block":   with("cert", "-----BEGIN CERTIFICATE-----\n"+good),
		"lone Name":       strings.Replace(with("name", "macbook"), `"name":`, `"Name":`, 1),
		"name and Name":   strings.Replace(with("name", "macbook"), `"name":"macbook"`, `"name":"macbook","Name":"air"`, 1),
		"Name then name":  strings.Replace(with("name", "macbook"), `"name":"macbook"`, `"Name":"air","name":"macbook"`, 1),
		"NODE":            strings.Replace(with("name", "macbook"), `"node":`, `"NODE":`, 1),
		"rsa":             with("cert", selfSigned(t, rsaKey, &rsaKey.PublicKey)),
		"p384":            with("cert", selfSigned(t, p384, &p384.PublicKey)),
		"garbage der":     with("cert", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")}))),
		"empty":           "",
		"null":            "null",
		"string":          `"macbook"`,
		"nested cert obj": with("cert", map[string]string{"pem": good}),
	}
	for name, in := range bad {
		if _, _, err := ParseAddRequest(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Right at the cap is fine; one byte over isn't.
	ok := with("name", "macbook")
	padded := ok + strings.Repeat(" ", MaxAddRequest-len(ok))
	if _, _, err := ParseAddRequest(strings.NewReader(padded)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if _, _, err := ParseAddRequest(strings.NewReader(padded + " ")); err == nil {
		t.Fatal("over the cap accepted")
	}
}
