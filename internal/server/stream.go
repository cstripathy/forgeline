package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cstripathy/forgeline/internal/store"
)

// keepAliveEvery is how often an idle stream sends a comment line so proxies keep it open.
const keepAliveEvery = 15 * time.Second

// LogEvent is the JSON payload of a log event in the SSE stream. Seq is also sent as the SSE
// id, so a client that reconnects with Last-Event-ID resumes after the last chunk it saw.
type LogEvent struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// EndEvent is the payload of the final "end" event: the step's terminal status.
type EndEvent struct {
	Status string `json:"status"`
}

// streamLogs replays the stored log, then follows new chunks until the step finishes.
//
// The step's status is read before the chunks. Output is written before a step is marked
// finished, so a chunk list that comes back empty after a terminal status really is the end.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, runID, stepID string) {
	var after int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "Last-Event-ID must be a non-negative integer")
			return
		}
		after = n
	}
	if _, err := s.stepStatus(r, runID, stepID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such run or step")
			return
		}
		s.internalError(w, "reading step", err)
		return
	}

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask reverse proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	poll := time.NewTicker(s.cfg.LogPollInterval)
	defer poll.Stop()
	lastSent := time.Now()
	for {
		status, err := s.stepStatus(r, runID, stepID)
		if err != nil {
			return // client gone or store failure; the client reconnects with Last-Event-ID
		}
		for {
			chunks, err := s.cfg.Store.ReadLog(r.Context(), runID, stepID, after, 200)
			if err != nil {
				return
			}
			for _, c := range chunks {
				payload, _ := json.Marshal(LogEvent{Seq: c.Seq, Text: string(c.Data)})
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", c.Seq, payload)
				after = c.Seq
			}
			if len(chunks) > 0 {
				rc.Flush()
				lastSent = time.Now()
			}
			if len(chunks) < 200 {
				if len(chunks) == 0 && status.Terminal() {
					payload, _ := json.Marshal(EndEvent{Status: string(status)})
					fmt.Fprintf(w, "event: end\ndata: %s\n\n", payload)
					rc.Flush()
					return
				}
				break
			}
		}
		if time.Since(lastSent) >= keepAliveEvery {
			fmt.Fprint(w, ": keep-alive\n\n")
			rc.Flush()
			lastSent = time.Now()
		}
		select {
		case <-poll.C:
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) stepStatus(r *http.Request, runID, stepID string) (store.StepStatus, error) {
	_, steps, err := s.cfg.Store.GetRun(r.Context(), runID)
	if err != nil {
		return "", err
	}
	for _, st := range steps {
		if st.StepID == stepID {
			return st.Status, nil
		}
	}
	return "", store.ErrNotFound
}
