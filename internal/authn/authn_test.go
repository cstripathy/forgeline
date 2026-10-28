package authn

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	Iterations = 2000 // the real value is slow, and the work factor is not what these tests check
	os.Exit(m.Run())
}

func TestDefaultWorkFactorIsNotWeakened(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// Guards the shipped constant: TestMain lowers the variable for speed, so check the source default.
	if defaultIterations < 600_000 {
		t.Fatalf("default PBKDF2 iterations = %d, want at least 600000", defaultIterations)
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "correct") {
		t.Fatal("hash contains the password")
	}
	if ok, rehash := VerifyPassword("correct horse battery staple", h); !ok || rehash {
		t.Fatalf("right password: ok=%v rehash=%v", ok, rehash)
	}
	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", h} {
		if ok, _ := VerifyPassword(wrong, h); ok {
			t.Errorf("wrong password %q accepted", wrong)
		}
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Fatal("two hashes of one password are equal; the salt is not random")
	}
}

func TestVerifyFlagsOldHashesAndRejectsMalformed(t *testing.T) {
	old, _ := hashWith("a-long-enough-password", make([]byte, 16), 1000)
	if ok, rehash := VerifyPassword("a-long-enough-password", old); !ok || !rehash {
		t.Fatalf("old-iteration hash: ok=%v rehash=%v, want true true", ok, rehash)
	}
	for _, bad := range []string{"", "plain", "md5$1$a$b", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$0$a$b",
		"pbkdf2-sha256$99999999999$a$b", "pbkdf2-sha256$1000$!!$b", "pbkdf2-sha256$1000$AAAA$!!"} {
		if ok, _ := VerifyPassword("anything", bad); ok {
			t.Errorf("malformed hash %q verified", bad)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                  false,
		"aaaaaaaaaaaaaaaa":       false,
		"alice-the-user":         false, // fine length, but see next case
		"a good long password":   true,
		strings.Repeat("x", 300): false,
	} {
		err := CheckPassword("someone-else", pw)
		if pw == "alice-the-user" {
			ok = true
		}
		if (err == nil) != ok {
			t.Errorf("CheckPassword(%.20q) = %v, want ok=%v", pw, err, ok)
		}
	}
	if err := CheckPassword("alice-the-user", "ALICE-THE-USER"); err == nil {
		t.Error("password equal to the username (any case) accepted")
	}
}

func TestTokens(t *testing.T) {
	tok, id, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if id == tok || TokenID(tok) != id || len(id) != 64 {
		t.Fatalf("token %q id %q", tok, id)
	}
	tok2, id2, _ := NewToken()
	if tok == tok2 || id == id2 {
		t.Fatal("tokens repeat")
	}
}

func TestRoles(t *testing.T) {
	if !Admin.AtLeast(Developer) || !Developer.AtLeast(Viewer) || Viewer.AtLeast(Developer) || Developer.AtLeast(Admin) {
		t.Fatal("role ordering is wrong")
	}
	if Role("root").AtLeast(Viewer) || Role("").AtLeast(Viewer) {
		t.Fatal("an unknown role must grant nothing")
	}
	if Admin.AtLeast("bogus") {
		t.Fatal("asking for an unknown role must not succeed")
	}
	if !Admin.Valid() || Role("root").Valid() {
		t.Fatal("Valid is wrong")
	}
}

func TestValidUsername(t *testing.T) {
	for name, ok := range map[string]bool{"ada": true, "ada.lovelace@x-y_z": true, "ab": false, "a b c": false,
		"semi;colon": false, "": false, strings.Repeat("a", 41): false, "ünï": false} {
		if (ValidUsername(name) == nil) != ok {
			t.Errorf("ValidUsername(%q) ok=%v, want %v", name, !ok, ok)
		}
	}
}
