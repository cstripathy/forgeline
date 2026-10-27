package secrets

import (
	"strings"
	"testing"
)

func TestFromEnviron(t *testing.T) {
	got, err := FromEnviron([]string{
		"PATH=/usr/bin",
		"FORGELINE_TOKEN=not-a-step-secret-token",
		"FORGELINE_SECRET_NPM_TOKEN=abcdefgh12345",
		"FORGELINE_SECRET__key=has=equals=signs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["NPM_TOKEN"] != "abcdefgh12345" || got["_key"] != "has=equals=signs" {
		t.Fatalf("got %v", got)
	}
}

func TestFromEnvironRejectsWithoutLeakingValues(t *testing.T) {
	for name, kv := range map[string]string{
		"too short":    "FORGELINE_SECRET_A=short",
		"newline":      "FORGELINE_SECRET_A=line1-long\nline2-long",
		"empty":        "FORGELINE_SECRET_A=",
		"no name":      "FORGELINE_SECRET_=longenoughvalue",
		"bad name":     "FORGELINE_SECRET_1A=longenoughvalue",
		"dash in name": "FORGELINE_SECRET_A-B=longenoughvalue",
	} {
		_, err := FromEnviron([]string{kv})
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		_, value, _ := strings.Cut(kv, "=")
		if value != "" && strings.Contains(err.Error(), value) {
			t.Errorf("%s: error leaks the value: %v", name, err)
		}
	}
}
