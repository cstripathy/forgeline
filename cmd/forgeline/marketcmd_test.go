package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarketKeygenAndSignRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var out, errb bytes.Buffer
	if code := runMarket([]string{"keygen", "acme"}, &out, &errb); code != 0 {
		t.Fatalf("keygen = %d: %s", code, errb.String())
	}
	info, err := os.Stat("acme.key")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	line := out.String()[strings.Index(out.String(), "FORGELINE_MARKET_KEYS=")+len("FORGELINE_MARKET_KEYS="):]
	id, b64, _ := strings.Cut(strings.TrimSpace(line), "=")
	pub, err := base64.StdEncoding.DecodeString(b64)
	if id != "acme" || err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("printed key line = %q", line)
	}
	// Never overwrite a key.
	if code := runMarket([]string{"keygen", "acme"}, &out, &errb); code == 0 {
		t.Error("keygen overwrote an existing key")
	}
	manifest := `{"id":"demo","version":1,"title":"Demo","description":"d","publisher":"Acme","tags":[],"skills":[{"key":"s","name":"A skill","description":"d","body":"Do the thing."}],"agents":[],"sha256":""}`
	os.WriteFile("demo.json", []byte(manifest), 0o644)
	out.Reset()
	errb.Reset()
	if code := runMarket([]string{"sign", "-key", "acme.key", "demo.json"}, &out, &errb); code != 0 {
		t.Fatalf("sign = %d: %s", code, errb.String())
	}
	sig, _ := os.ReadFile(filepath.Join(dir, "demo.sig"))
	signed, _ := os.ReadFile(filepath.Join(dir, "demo.json"))
	if !strings.Contains(string(sig), `"key_id": "acme"`) || strings.Contains(string(signed), `"sha256": ""`) {
		t.Errorf("sig = %s\nmanifest = %s", sig, signed)
	}
	// A manifest that is not a valid package is not signed.
	os.WriteFile("bad.json", []byte(`{"id":"bad"}`), 0o644)
	if code := runMarket([]string{"sign", "-key", "acme.key", "bad.json"}, &out, &errb); code == 0 {
		t.Error("an invalid package was signed")
	}
	if _, err := os.Stat("bad.sig"); err == nil {
		t.Error("a signature was written for an invalid package")
	}
	for _, args := range [][]string{{}, {"nope"}, {"keygen"}, {"keygen", "../evil"}, {"keygen", "-x"}, {"sign"}, {"sign", "-key", "acme.key"}} {
		if runMarket(args, &out, &errb) == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
}
