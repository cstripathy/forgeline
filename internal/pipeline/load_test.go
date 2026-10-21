package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileValid(t *testing.T) {
	p, err := LoadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("LoadFile() = %v, want nil", err)
	}
	if p.Name != "demo" || len(p.Steps) != 2 || p.Steps[1].Needs[0] != "build" {
		t.Fatalf("unexpected pipeline: %+v", p)
	}
}

func TestLoadFileErrors(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		{"syntax-error.yaml", []string{"syntax-error.yaml", "line"}},
		{"unknown-field.yaml", []string{"nope"}},
		{"empty.yaml", []string{"version", "name", "steps"}},
		{"invalid-multi.yaml", []string{"name:", "steps[0]:", "steps[1].needs[0]: unknown step", "steps[1].timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			_, err := LoadFile(filepath.Join("testdata", tt.file))
			if err == nil {
				t.Fatal("LoadFile() = nil, want error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLoadReturnsValidationErrorUnchanged(t *testing.T) {
	_, err := LoadFile(filepath.Join("testdata", "invalid-multi.yaml"))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) < 3 {
		t.Fatalf("want *ValidationError with several problems, got %T %v", err, err)
	}
}

func TestLoadFileMissing(t *testing.T) {
	_, err := LoadFile(filepath.Join("testdata", "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}
