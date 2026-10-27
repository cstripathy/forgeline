package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newVault(t *testing.T) *Vault {
	t.Helper()
	key := bytes.Repeat([]byte{7}, KeySize)
	v, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSealOpenRoundTrip(t *testing.T) {
	v := newVault(t)
	for _, val := range []string{"x", "a longer secret with spaces", "unicode ✓ ключ", strings.Repeat("A", 100000), ""} {
		sealed, err := v.Seal("NAME", val)
		if err != nil {
			t.Fatal(err)
		}
		if len(val) >= 8 && strings.Contains(sealed, val) {
			t.Fatalf("sealed text contains the plaintext")
		}
		got, err := v.Open("NAME", sealed)
		if err != nil || got != val {
			t.Fatalf("Open = %q, %v; want %q", got, err, val)
		}
	}
}

func TestSealIsRandomised(t *testing.T) {
	v := newVault(t)
	a, _ := v.Seal("N", "same value")
	b, _ := v.Seal("N", "same value")
	if a == b {
		t.Fatal("two seals of one value are identical; the nonce is not random")
	}
}

func TestOpenRejectsWrongNameKeyAndDamage(t *testing.T) {
	v := newVault(t)
	sealed, _ := v.Seal("DEPLOY_KEY", "s3cret-value")

	if _, err := v.Open("OTHER_NAME", sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("moved to another name: %v, want ErrCorrupt", err)
	}
	other, _ := New(bytes.Repeat([]byte{9}, KeySize))
	if _, err := other.Open("DEPLOY_KEY", sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("wrong key: %v, want ErrCorrupt", err)
	}
	damaged := []byte(sealed)
	if damaged[len(damaged)-6] == 'A' {
		damaged[len(damaged)-6] = 'B'
	} else {
		damaged[len(damaged)-6] = 'A'
	}
	if _, err := v.Open("DEPLOY_KEY", string(damaged)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("flipped byte: %v, want ErrCorrupt", err)
	}
	for _, junk := range []string{"", "!!!", "AAAA"} {
		if _, err := v.Open("DEPLOY_KEY", junk); !errors.Is(err, ErrCorrupt) {
			t.Errorf("junk %q: %v, want ErrCorrupt", junk, err)
		}
	}
}

func TestNewRejectsBadKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Errorf("key of %d bytes accepted", n)
		}
	}
}

func TestParseKey(t *testing.T) {
	good := "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=" // 32 bytes of 7
	if k, err := ParseKey(good + "\n"); err != nil || !bytes.Equal(k, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("ParseKey = %v, %v", k, err)
	}
	for _, bad := range []string{"", "short", "BwcH", strings.Repeat("A", 100)} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, created, err := LoadOrCreateKey("", path)
	if err != nil || !created || len(k1) != KeySize {
		t.Fatalf("first call: %d bytes, created=%v, err=%v", len(k1), created, err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	k2, created, err := LoadOrCreateKey("", path)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatalf("second call must reload the same key: created=%v err=%v", created, err)
	}
	// The environment wins over the file.
	envKey := "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="
	k3, created, err := LoadOrCreateKey(envKey, path)
	if err != nil || created || bytes.Equal(k3, k1) {
		t.Fatalf("env key not used: created=%v err=%v", created, err)
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	if _, _, err := LoadOrCreateKey("", path); err == nil {
		t.Fatal("a corrupt key file must be an error, never silently replaced")
	}
}
