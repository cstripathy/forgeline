package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cstripathy/forgeline/internal/pipeline"
	"github.com/cstripathy/forgeline/internal/store"
)

// maxPipelineBytes caps the pipeline YAML in a submission.
const maxPipelineBytes = 1 << 20

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /v1/runs", s.auth(http.HandlerFunc(s.handleSubmit)))
	mux.Handle("GET /v1/runs", s.auth(http.HandlerFunc(s.handleList)))
	mux.Handle("GET /v1/runs/{id}", s.auth(http.HandlerFunc(s.handleGet)))
	mux.Handle("POST /v1/runs/{id}/cancel", s.auth(http.HandlerFunc(s.handleCancel)))
	mux.Handle("GET /v1/runs/{id}/steps/{step}/logs", s.auth(http.HandlerFunc(s.handleLogs)))
	return mux
}

// auth requires "Authorization: Bearer <token>". Both sides are hashed first so the
// comparison takes the same time whatever the token's length or contents.
func (s *Server) auth(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(s.cfg.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorJSON{Error: msg})
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error: "+what)
}

// handleSubmit accepts multipart/form-data with a "pipeline" field (YAML) and an optional
// "source" file (tar.gz of the workspace).
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxSourceBytes+maxPipelineBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a pipeline field: "+err.Error())
		return
	}

	var yamlText string
	var haveYAML bool
	var archive string // path of the uploaded archive in scratch space
	defer func() {
		if archive != "" {
			os.Remove(archive)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.badBody(w, err)
			return
		}
		switch part.FormName() {
		case "pipeline":
			b, err := io.ReadAll(io.LimitReader(part, maxPipelineBytes+1))
			if err != nil {
				s.badBody(w, err)
				return
			}
			if len(b) > maxPipelineBytes {
				writeError(w, http.StatusRequestEntityTooLarge, "pipeline is larger than 1 MiB")
				return
			}
			yamlText, haveYAML = string(b), true
		case "source":
			if archive, err = s.saveUpload(part); err != nil {
				s.badBody(w, err)
				return
			}
		}
	}
	if !haveYAML {
		writeError(w, http.StatusBadRequest, `missing "pipeline" field`)
		return
	}

	p, err := pipeline.Load(strings.NewReader(yamlText))
	if err == nil {
		_, err = pipeline.Plan(p)
	}
	if err != nil {
		var ve *pipeline.ValidationError
		if errors.As(err, &ve) {
			body := ErrorJSON{Error: "invalid pipeline"}
			for _, fe := range ve.Errors {
				body.Problems = append(body.Problems, ProblemJSON{Path: fe.Path, Msg: fe.Msg})
			}
			writeJSON(w, http.StatusBadRequest, body)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Unpack into scratch space first so a bad archive creates no run.
	ws, err := os.MkdirTemp(filepath.Join(s.cfg.DataDir, "tmp"), "ws-")
	if err != nil {
		s.internalError(w, "preparing workspace", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(ws)
		}
	}()
	if archive != "" {
		f, err := os.Open(archive)
		if err != nil {
			s.internalError(w, "reading upload", err)
			return
		}
		err = ExtractTarGz(f, ws, s.cfg.MaxSourceBytes)
		f.Close()
		if errors.Is(err, ErrBadArchive) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			s.internalError(w, "unpacking source", err)
			return
		}
	}

	stepIDs := make([]string, len(p.Steps))
	for i, st := range p.Steps {
		stepIDs[i] = st.ID
	}
	run, err := s.cfg.Store.CreateRun(r.Context(), store.Run{
		PipelineName: p.Name, PipelineYAML: yamlText, Trigger: store.TriggerAPI,
	}, stepIDs)
	if err != nil {
		s.internalError(w, "creating run", err)
		return
	}
	if err := os.Rename(ws, s.workspace(run.ID)); err != nil {
		s.internalError(w, "placing workspace", err)
		s.failQueued(run.ID, "could not place workspace")
		return
	}
	committed = true

	select {
	case s.wake <- struct{}{}:
	default:
	}
	got, steps, err := s.cfg.Store.GetRun(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, "reading run", err)
		return
	}
	w.Header().Set("Location", "/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, runJSON(got, steps))
}

// saveUpload copies the source part to scratch space, enforcing the size limit.
func (s *Server) saveUpload(part *multipart.Part) (string, error) {
	f, err := os.CreateTemp(filepath.Join(s.cfg.DataDir, "tmp"), "upload-*.tar.gz")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(part, s.cfg.MaxSourceBytes+1))
	cerr := f.Close()
	switch {
	case err != nil:
		os.Remove(f.Name())
		return "", err
	case cerr != nil:
		os.Remove(f.Name())
		return "", cerr
	case n > s.cfg.MaxSourceBytes:
		os.Remove(f.Name())
		return "", errTooLarge
	}
	return f.Name(), nil
}

var errTooLarge = errors.New("source archive is larger than the server's limit")

func (s *Server) badBody(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.Is(err, errTooLarge) || errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is larger than the server's limit")
		return
	}
	writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
}

// failQueued is best effort: it cancels a run whose workspace could not be placed.
func (s *Server) failQueued(id, reason string) {
	if _, err := s.cfg.Store.UpdateRun(context.WithoutCancel(context.Background()), id, store.RunUpdate{Status: store.RunCanceled, Reason: reason}); err != nil {
		s.log.Error("canceling run after failed setup", "run", id, "err", err)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	opts := store.ListOptions{Before: r.URL.Query().Get("before")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	runs, err := s.cfg.Store.ListRuns(r.Context(), opts)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown before cursor")
		return
	}
	if err != nil {
		s.internalError(w, "listing runs", err)
		return
	}
	out := ListJSON{Runs: make([]RunJSON, len(runs))}
	for i, run := range runs {
		out.Runs[i] = runJSON(run, nil)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(runs) > 0 && len(runs) >= min(limit, 200) {
		out.NextBefore = runs[len(runs)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	run, steps, err := s.cfg.Store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		s.internalError(w, "reading run", err)
		return
	}
	writeJSON(w, http.StatusOK, runJSON(run, steps))
}

// handleCancel is idempotent: canceling a finished run returns it unchanged.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, steps, err := s.cfg.Store.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		s.internalError(w, "reading run", err)
		return
	}
	status := http.StatusOK
	switch {
	case run.Status.Terminal():
	case run.Status == store.RunQueued:
		_, err := s.cfg.Store.UpdateRun(r.Context(), id, store.RunUpdate{Status: store.RunCanceled, Reason: "canceled by user"})
		switch {
		case err == nil:
			for _, st := range steps {
				s.cfg.Store.UpdateStep(r.Context(), id, st.StepID, store.StepUpdate{Status: store.StepCanceled})
			}
		case errors.Is(err, store.ErrInvalidTransition):
			// A worker claimed it just now; cancel it as a running run.
			s.requestCancel(id)
			status = http.StatusAccepted
		default:
			s.internalError(w, "canceling run", err)
			return
		}
	default:
		s.requestCancel(id)
		status = http.StatusAccepted
	}
	run, steps, err = s.cfg.Store.GetRun(r.Context(), id)
	if err != nil {
		s.internalError(w, "reading run", err)
		return
	}
	writeJSON(w, status, runJSON(run, steps))
}

// handleLogs returns the step's stored log as plain text, or streams it live as
// Server-Sent Events when the client asks for text/event-stream.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	runID, stepID := r.PathValue("id"), r.PathValue("step")
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamLogs(w, r, runID, stepID)
		return
	}
	var after int64
	wrote := false
	for {
		chunks, err := s.cfg.Store.ReadLog(r.Context(), runID, stepID, after, 500)
		if errors.Is(err, store.ErrNotFound) && !wrote {
			writeError(w, http.StatusNotFound, "no such run or step")
			return
		}
		if err != nil {
			if !wrote {
				s.internalError(w, "reading log", err)
			}
			return
		}
		if !wrote {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		if len(chunks) == 0 {
			return
		}
		for _, c := range chunks {
			w.Write(c.Data)
			after = c.Seq
		}
	}
}
