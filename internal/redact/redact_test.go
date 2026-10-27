package redact

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func run(t *testing.T, values []string, writes ...string) string {
	t.Helper()
	var out bytes.Buffer
	w := New(&out, values)
	for _, s := range writes {
		n, err := w.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestMasks(t *testing.T) {
	const sec = "s3cr3t-value"
	tests := []struct {
		name   string
		values []string
		writes []string
		want   string
	}{
		{"no values passes through", nil, []string{"hello ", "world"}, "hello world"},
		{"empty value is ignored", []string{""}, []string{"abc"}, "abc"},
		{"one occurrence", []string{sec}, []string{"token=" + sec + "\n"}, "token=***\n"},
		{"two occurrences", []string{sec}, []string{sec + " and " + sec}, "*** and ***"},
		{"split in two writes", []string{sec}, []string{"token=s3cr3t", "-value\n"}, "token=***\n"},
		{"split one byte at a time", []string{sec}, strings.Split("x"+sec+"y", ""), "x***y"},
		{"partial prefix at the end is released on close", []string{sec}, []string{"ends with s3cr"}, "ends with s3cr"},
		{"near miss is not masked", []string{sec}, []string{"s3cr3t-valu", "E"}, "s3cr3t-valuE"},
		{"two values", []string{"aaaa1111", "bbbb2222"}, []string{"aaaa1111 bbbb2222"}, "*** ***"},
		{"longer value wins when one contains the other", []string{"abcdefgh", "abcdefghij"}, []string{"abcdefghij"}, "***"},
		{"false start then real value", []string{"abcd1234"}, []string{"abcabcd1234"}, "abc***"},
		{"binary-safe", []string{"\x00\x01secret"}, []string{"a\x00\x01secret\xff"}, "a***\xff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(t, tt.values, tt.writes...); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Held-back bytes must not delay output that cannot be part of a value.
func TestDoesNotHoldBackUnrelatedOutput(t *testing.T) {
	var out bytes.Buffer
	w := New(&out, []string{"s3cr3t-value"})
	w.Write([]byte("plain line\n"))
	if got := out.String(); got != "plain line\n" {
		t.Fatalf("before Close got %q, want the line written at once", got)
	}
}

func TestEveryByteSplitNeverLeaksValue(t *testing.T) {
	const sec = "p4ssw0rd!!"
	input := "a " + sec + " b " + sec[:5] + " c " + sec + sec + "\n"
	for cut := 1; cut < len(input); cut++ {
		got := run(t, []string{sec}, input[:cut], input[cut:])
		if strings.Contains(got, sec) {
			t.Fatalf("split at %d leaked the value: %q", cut, got)
		}
		if want := "a *** b " + sec[:5] + " c ******\n"; got != want {
			t.Fatalf("split at %d: got %q, want %q", cut, got, want)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestPropagatesWriteErrors(t *testing.T) {
	w := New(failWriter{}, []string{"secretvalue"})
	if _, err := w.Write([]byte("hello\n")); err == nil {
		t.Fatal("want the underlying error")
	}
	if _, err := w.Write([]byte("again")); err == nil {
		t.Fatal("want the error to stick")
	}
}
