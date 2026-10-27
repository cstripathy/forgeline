// Package cache skips steps whose inputs have not changed since their last success.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/pipeline"
)

// FormatVersion changes whenever the key definition does, so old records stop matching.
const FormatVersion = "2" // 2: the network permission joined the key

// Key computes the cache key for a step, as defined in docs/design/002-spec-extensions.md.
// It reads the input files now, so call it just before the step would run.
func Key(s executor.StepRun) (string, error) {
	h := sha256.New()
	field(h, "format", FormatVersion)
	field(h, "spec", pipeline.VersionV1Alpha1)
	field(h, "image", s.Image)
	if s.Script != "" {
		field(h, "script", s.Script)
	} else {
		field(h, "command", fmt.Sprint(len(s.Command)))
		for _, c := range s.Command {
			field(h, "arg", c)
		}
	}

	names := make([]string, 0, len(s.Env))
	for k := range s.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		field(h, "env", k)
		field(h, "val", s.Env[k])
	}

	// A step cut off from the network can compute something different from one that is not.
	network := s.Network
	if network == "" {
		network = pipeline.NetworkEgress
	}
	field(h, "network", network)

	workdir := s.WorkDir
	if workdir == "" {
		workdir = executor.WorkspaceMount
	}
	field(h, "workdir", workdir)

	files, err := matchFiles(s.Workspace, s.Inputs)
	if err != nil {
		return "", fmt.Errorf("step %q: reading inputs: %w", s.ID, err)
	}
	for _, rel := range files {
		sum, err := hashFile(filepath.Join(s.Workspace, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("step %q: hashing %s: %w", s.ID, rel, err)
		}
		field(h, "file", rel)
		field(h, "sha", sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// field writes a label and value, each length-prefixed, so fields cannot run together.
func field(h hash.Hash, label, value string) {
	for _, part := range []string{label, value} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		io.WriteString(h, part)
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
