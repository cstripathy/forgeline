// Package redact masks secret values in a stream of log output.
//
// Masking is exact-match: a value is replaced where it appears verbatim, including when it is
// split across writes. A step that prints a transformed value (base64, URL-encoded, reversed,
// one character per line) defeats it. It limits accidents, it is not a boundary; the boundary
// is that only steps that list a secret receive it. See docs/design/005-threat-model.md (T3).
package redact

import (
	"io"
	"sort"
	"sync"
)

// Mask replaces every masked value.
const Mask = "***"

// Writer wraps an io.Writer and masks values before they reach it. Call Close after the last
// write: output that might be the start of a value is held back until then.
type Writer struct {
	mu      sync.Mutex
	out     io.Writer
	values  [][]byte // longest first, so a value that contains another is masked whole
	pending []byte   // held back: a suffix that could still become a value
	err     error
}

// New returns a Writer that masks the given values. Empty values are ignored. With no
// values it passes everything through unchanged and holds nothing back.
func New(out io.Writer, values []string) *Writer {
	w := &Writer{out: out}
	for _, v := range values {
		if v != "" {
			w.values = append(w.values, []byte(v))
		}
	}
	sort.SliceStable(w.values, func(i, j int) bool { return len(w.values[i]) > len(w.values[j]) })
	return w
}

// Write masks p and forwards it. It reports len(p) on success so callers see no short write.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if len(w.values) == 0 {
		return w.forward(p, len(p))
	}
	buf := append(w.pending, p...)
	w.pending = nil
	buf = w.replace(buf)
	keep := w.heldBack(buf)
	emit := buf[:len(buf)-keep]
	w.pending = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(emit) > 0 {
		if _, err := w.out.Write(emit); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}

// Close writes whatever was held back. The held bytes did not complete a value, so they are
// written as they are. Close does not close the underlying writer.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return w.err
	}
	rest := w.pending
	w.pending = nil
	if w.err != nil {
		return w.err
	}
	_, w.err = w.out.Write(rest)
	return w.err
}

func (w *Writer) forward(p []byte, n int) (int, error) {
	if _, err := w.out.Write(p); err != nil {
		w.err = err
		return 0, err
	}
	return n, nil
}

// replace masks every complete occurrence of every value in buf, scanning left to right so
// that overlapping values resolve to the earliest, then longest, match.
func (w *Writer) replace(buf []byte) []byte {
	var out []byte
	for i := 0; i < len(buf); {
		matched := 0
		for _, v := range w.values {
			if len(v) <= len(buf)-i && string(buf[i:i+len(v)]) == string(v) {
				matched = len(v)
				break // values are longest first
			}
		}
		if matched > 0 {
			out = append(out, Mask...)
			i += matched
			continue
		}
		out = append(out, buf[i])
		i++
	}
	return out
}

// heldBack returns how many bytes at the end of buf are a proper prefix of some value, and
// so cannot be written yet. It is the longest such suffix.
func (w *Writer) heldBack(buf []byte) int {
	best := 0
	for _, v := range w.values {
		max := len(v) - 1
		if max > len(buf) {
			max = len(buf)
		}
		for n := max; n > best; n-- {
			if string(buf[len(buf)-n:]) == string(v[:n]) {
				best = n
				break
			}
		}
	}
	return best
}
