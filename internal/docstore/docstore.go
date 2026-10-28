// Package docstore keeps the console's small records: users, sessions, saved pipelines,
// encrypted secrets, integration settings and the audit log. A record is a JSON document
// addressed by (kind, id). Runs and logs live in the run store, not here.
//
// Memory serves a laptop (optionally saved to a file); Postgres serves a server.
package docstore

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrNotFound is returned when a document does not exist.
var ErrNotFound = errors.New("docstore: not found")

// ErrExists is returned by Create when the document already exists.
var ErrExists = errors.New("docstore: already exists")

// Doc is a stored document with its address.
type Doc struct {
	Kind string
	ID   string
	Body json.RawMessage
}

// ListOptions bounds and orders a listing. Documents are ordered by id.
type ListOptions struct {
	// Limit caps the result. Zero means no limit.
	Limit int
	// Desc lists the highest id first.
	Desc bool
}

// Store is a document store. Implementations are safe for concurrent use.
type Store interface {
	// Create stores v as a new document, or returns ErrExists.
	Create(ctx context.Context, kind, id string, v any) error
	// Put stores v, replacing any existing document.
	Put(ctx context.Context, kind, id string, v any) error
	// Get decodes a document into out, or returns ErrNotFound.
	Get(ctx context.Context, kind, id string, out any) error
	// List returns the documents of one kind.
	List(ctx context.Context, kind string, opts ListOptions) ([]Doc, error)
	// Delete removes a document, or returns ErrNotFound.
	Delete(ctx context.Context, kind, id string) error
	// Count returns how many documents of a kind exist.
	Count(ctx context.Context, kind string) (int, error)
	Close()
}

// Decode decodes every document in docs into a T, skipping none: a document that does not
// decode is an error, because silently dropping a user or a secret would be worse.
func Decode[T any](docs []Doc) ([]T, error) {
	out := make([]T, 0, len(docs))
	for _, d := range docs {
		var v T
		if err := json.Unmarshal(d.Body, &v); err != nil {
			return nil, errors.New("docstore: decoding " + d.Kind + "/" + d.ID + ": " + err.Error())
		}
		out = append(out, v)
	}
	return out, nil
}
