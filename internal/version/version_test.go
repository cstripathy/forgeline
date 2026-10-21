package version

import "testing"

func TestStringIncludesName(t *testing.T) {
	got := String()
	want := "forgeline " + Version
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
