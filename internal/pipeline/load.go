package pipeline

import (
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads a pipeline from YAML, rejects unknown fields, and validates it. Validation
// problems are returned as a *ValidationError, unchanged.
//
// Validation errors carry field paths (steps[2].needs[0]), not line numbers, because the
// YAML is converted to JSON before decoding. YAML syntax errors do include a line number.
func Load(r io.Reader) (*Pipeline, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading pipeline: %w", err)
	}
	var p Pipeline
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return nil, fmt.Errorf("parsing pipeline: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadFile is Load for a file path. Errors name the file.
func LoadFile(path string) (*Pipeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p, err := Load(f)
	if err != nil {
		if ve, ok := err.(*ValidationError); ok {
			return nil, ve
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
