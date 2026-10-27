package store

import "testing"

func TestMemory(t *testing.T) {
	runSuite(t, func(t *testing.T) Store { return NewMemory() })
}
