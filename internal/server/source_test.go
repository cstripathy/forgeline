package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name     string
	typeflag byte
	body     string
	mode     int64
	link     string
}

func makeArchive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.typeflag == 0 {
			e.typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Linkname: e.link}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractTarGzAcceptsFilesAndDirs(t *testing.T) {
	dst := t.TempDir()
	arc := makeArchive(t,
		entry{name: "./", typeflag: tar.TypeDir},
		entry{name: "src/", typeflag: tar.TypeDir},
		entry{name: "src/a.txt", body: "hello", mode: 0o4666}, // setuid bit must be dropped
		entry{name: "run.sh", body: "#!/bin/sh", mode: 0o755},
	)
	if err := ExtractTarGz(bytes.NewReader(arc), dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "src", "a.txt")); string(b) != "hello" {
		t.Fatalf("a.txt = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "src", "a.txt")); fi.Mode().Perm() != 0o644 || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("a.txt mode = %v, want plain 0644", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dst, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, want 0755", fi.Mode())
	}
}

func TestExtractTarGzRejectsHostileArchives(t *testing.T) {
	tests := map[string][]entry{
		"parent traversal":   {{name: "../evil.txt", body: "x"}},
		"nested traversal":   {{name: "a/../../evil.txt", body: "x"}},
		"absolute path":      {{name: "/tmp/evil.txt", body: "x"}},
		"backslash":          {{name: `a\..\evil.txt`, body: "x"}},
		"symlink":            {{name: "link", typeflag: tar.TypeSymlink, link: "/etc/passwd"}},
		"hard link":          {{name: "hl", typeflag: tar.TypeLink, link: "/etc/passwd"}},
		"device":             {{name: "dev", typeflag: tar.TypeChar}},
		"duplicate name":     {{name: "a.txt", body: "1"}, {name: "a.txt", body: "2"}},
		"file under symlink": {{name: "d", typeflag: tar.TypeSymlink, link: "/tmp"}, {name: "d/evil.txt", body: "x"}},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dst := filepath.Join(parent, "ws")
			os.Mkdir(dst, 0o755)
			err := ExtractTarGz(bytes.NewReader(makeArchive(t, entries...)), dst, 1<<20)
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("err = %v, want ErrBadArchive", err)
			}
			if _, err := os.Stat(filepath.Join(parent, "evil.txt")); err == nil {
				t.Fatal("a file escaped the destination")
			}
		})
	}
}

func TestExtractTarGzLimitsAndGarbage(t *testing.T) {
	big := makeArchive(t, entry{name: "big", body: string(make([]byte, 2048))})
	if err := ExtractTarGz(bytes.NewReader(big), t.TempDir(), 1024); !errors.Is(err, ErrBadArchive) {
		t.Errorf("oversized archive: err = %v, want ErrBadArchive", err)
	}
	if err := ExtractTarGz(bytes.NewReader([]byte("not gzip")), t.TempDir(), 1024); !errors.Is(err, ErrBadArchive) {
		t.Errorf("garbage: err = %v, want ErrBadArchive", err)
	}
	if err := ExtractTarGz(bytes.NewReader(makeArchive(t)), t.TempDir(), 1024); err != nil {
		t.Errorf("empty archive: %v", err)
	}
}

func TestPackDirRoundTripSkipsExcluded(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "src"), 0o755)
	os.MkdirAll(filepath.Join(src, ".forgeline", "cache"), 0o755)
	os.MkdirAll(filepath.Join(src, ".git"), 0o755)
	os.WriteFile(filepath.Join(src, "src", "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(src, ".forgeline", "cache", "k"), []byte("k"), 0o644)
	os.WriteFile(filepath.Join(src, ".git", "config"), []byte("g"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(src, "link"))

	arc, err := PackDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(arc), dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "src", "a.txt")); string(b) != "a" {
		t.Error("src/a.txt missing after round trip")
	}
	for _, rel := range []string{".forgeline", ".git", "link"} {
		if _, err := os.Lstat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("%s should not be packed", rel)
		}
	}
}
