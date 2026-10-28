package docstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Memory is a Store held in memory. With a path it writes every change to that file, so a
// laptop keeps its users and pipelines across restarts. The file may hold secrets (encrypted)
// and password hashes, so it is created with mode 0600.
type Memory struct {
	mu   sync.RWMutex
	data map[string]map[string]json.RawMessage
	path string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{data: map[string]map[string]json.RawMessage{}} }

// OpenFile returns a Memory backed by path, loading it if it exists.
func OpenFile(path string) (*Memory, error) {
	m := NewMemory()
	m.path = path
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("docstore: reading %s: %w", path, err)
	}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m.data); err != nil {
		return nil, fmt.Errorf("docstore: %s is not a valid store file: %w", path, err)
	}
	return m, nil
}

func (m *Memory) Create(_ context.Context, kind, id string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[kind][id]; ok {
		return ErrExists
	}
	return m.set(kind, id, body)
}

func (m *Memory) Put(_ context.Context, kind, id string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set(kind, id, body)
}

// set stores body and saves the file. If saving fails the change is rolled back, so memory
// never claims a write the file does not have. The caller holds the lock.
func (m *Memory) set(kind, id string, body json.RawMessage) error {
	if m.data[kind] == nil {
		m.data[kind] = map[string]json.RawMessage{}
	}
	prev, had := m.data[kind][id]
	m.data[kind][id] = body
	if err := m.save(); err != nil {
		if had {
			m.data[kind][id] = prev
		} else {
			delete(m.data[kind], id)
		}
		return err
	}
	return nil
}

func (m *Memory) Get(_ context.Context, kind, id string, out any) error {
	m.mu.RLock()
	body, ok := m.data[kind][id]
	m.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	return json.Unmarshal(body, out)
}

func (m *Memory) List(_ context.Context, kind string, opts ListOptions) ([]Doc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.data[kind]))
	for id := range m.data[kind] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if opts.Desc {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	if opts.Limit > 0 && len(ids) > opts.Limit {
		ids = ids[:opts.Limit]
	}
	out := make([]Doc, len(ids))
	for i, id := range ids {
		out[i] = Doc{Kind: kind, ID: id, Body: append(json.RawMessage(nil), m.data[kind][id]...)}
	}
	return out, nil
}

func (m *Memory) Delete(_ context.Context, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.data[kind][id]
	if !ok {
		return ErrNotFound
	}
	delete(m.data[kind], id)
	if err := m.save(); err != nil {
		m.data[kind][id] = prev
		return err
	}
	return nil
}

func (m *Memory) Count(_ context.Context, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data[kind]), nil
}

func (m *Memory) Close() {}

// save writes the whole store atomically. The caller holds the lock.
func (m *Memory) save() error {
	if m.path == "" {
		return nil
	}
	b, err := json.Marshal(m.data)
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	tmp, err := os.CreateTemp(dir, ".docstore-*") // created 0600
	if err != nil {
		return fmt.Errorf("docstore: saving: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("docstore: saving: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("docstore: saving: %w", err)
	}
	if err := os.Rename(tmp.Name(), m.path); err != nil {
		return fmt.Errorf("docstore: saving: %w", err)
	}
	return nil
}
