package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxArchiveEntries bounds how many entries an uploaded archive may contain.
const maxArchiveEntries = 100_000

// ErrBadArchive is wrapped by errors caused by the uploaded archive itself, so the API can
// answer 400 rather than 500.
var ErrBadArchive = errors.New("invalid source archive")

// ExtractTarGz unpacks a gzip-compressed tar archive into dst, which must already exist and
// be empty. The archive is untrusted, so extraction is strict:
//
//   - only regular files and directories are allowed; symlinks, hard links and devices are rejected
//   - names must be relative and stay inside dst
//   - permissions are normalised (0644, or 0755 if the owner-execute bit is set); setuid and
//     similar bits are dropped
//   - at most maxBytes of file content and maxArchiveEntries entries are unpacked, which also
//     bounds decompression bombs
func ExtractTarGz(r io.Reader, dst string, maxBytes int64) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: not gzip data: %v", ErrBadArchive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var total int64
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		if n >= maxArchiveEntries {
			return fmt.Errorf("%w: more than %d entries", ErrBadArchive, maxArchiveEntries)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." {
			continue
		}
		if strings.ContainsRune(hdr.Name, '\\') || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("%w: entry %q is not a relative path inside the archive", ErrBadArchive, hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total+hdr.Size > maxBytes {
				return fmt.Errorf("%w: contents exceed %d bytes", ErrBadArchive, maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if hdr.Mode&0o100 != 0 {
				mode = 0o755
			}
			// O_EXCL: a repeated name is an error, never an overwrite.
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return fmt.Errorf("%w: entry %q: %v", ErrBadArchive, hdr.Name, err)
			}
			// Copy at most the declared size plus one byte, so a lying header is caught.
			n, err := io.Copy(f, io.LimitReader(tr, hdr.Size+1))
			cerr := f.Close()
			if err != nil {
				return fmt.Errorf("%w: reading entry %q: %v", ErrBadArchive, hdr.Name, err)
			}
			if cerr != nil {
				return cerr
			}
			if n > hdr.Size {
				return fmt.Errorf("%w: entry %q is larger than its header says", ErrBadArchive, hdr.Name)
			}
			total += n
		default:
			return fmt.Errorf("%w: entry %q has unsupported type %q (only files and directories are allowed)",
				ErrBadArchive, hdr.Name, string(rune(hdr.Typeflag)))
		}
	}
}

// packExcluded are directories PackDir leaves out.
var packExcluded = map[string]bool{".forgeline": true, ".git": true}

// PackDir returns a gzip-compressed tar archive of the regular files under dir. Symlinks
// and the directories in packExcluded are skipped.
func PackDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			if packExcluded[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
