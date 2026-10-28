package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/secrets"
)

const (
	kindSecret     = "secret"
	maxSecretBytes = 16 << 10
)

var secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretDoc is a sealed secret. Names containing a dot are internal (the GitHub token, for
// example): they can never be a step's secret name, so pipelines cannot request them.
type secretDoc struct {
	Name        string    `json:"name"`
	Sealed      string    `json:"sealed"`
	Description string    `json:"description,omitempty"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

func internalSecret(name string) bool { return strings.Contains(name, ".") }

// putSecret seals and stores a value, creating or rotating it.
func (s *Server) putSecret(ctx context.Context, actor, name, value, description string) (secretDoc, bool, error) {
	sealed, err := s.vault.Seal(name, value)
	if err != nil {
		return secretDoc{}, false, err
	}
	now := time.Now().UTC()
	var d secretDoc
	rotated := false
	switch err := s.docs.Get(ctx, kindSecret, name, &d); {
	case err == nil:
		rotated = true
		d.Sealed, d.Version, d.UpdatedAt, d.UpdatedBy = sealed, d.Version+1, now, actor
		if description != "" || value != "" {
			d.Description = description
		}
	case errors.Is(err, docstore.ErrNotFound):
		d = secretDoc{Name: name, Sealed: sealed, Description: description, Version: 1, CreatedAt: now, CreatedBy: actor, UpdatedAt: now, UpdatedBy: actor}
	default:
		return secretDoc{}, false, err
	}
	return d, rotated, s.docs.Put(ctx, kindSecret, name, d)
}

// internalSecretValue reads an internal secret such as the GitHub token. ok is false if unset.
func (s *Server) internalSecretValue(ctx context.Context, name string) (string, bool) {
	var d secretDoc
	if err := s.docs.Get(ctx, kindSecret, name, &d); err != nil {
		return "", false
	}
	v, err := s.vault.Open(name, d.Sealed)
	if err != nil {
		s.log.Error("opening an internal secret", "name", name, "err", err)
		return "", false
	}
	return v, true
}

// stepSecrets is every value a step may be given: the console's vault plus the environment
// variables, the environment winning. Internal secrets are never included.
func (s *Server) stepSecrets(ctx context.Context) map[string]string {
	out := map[string]string{}
	docs, err := s.docs.List(ctx, kindSecret, docstore.ListOptions{})
	if err != nil {
		s.log.Error("listing secrets", "err", err)
	}
	for _, doc := range docs {
		if internalSecret(doc.ID) {
			continue
		}
		var d secretDoc
		if err := jsonUnmarshal(doc.Body, &d); err != nil {
			continue
		}
		v, err := s.vault.Open(d.Name, d.Sealed)
		if err != nil {
			s.log.Error("a stored secret cannot be opened; is the master key the one it was sealed with?", "name", d.Name)
			continue
		}
		out[d.Name] = v
	}
	for k, v := range s.cfg.Secrets {
		out[k] = v
	}
	return out
}

// secretNames is the set of names a step could request now.
func (s *Server) secretNames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	docs, err := s.docs.List(ctx, kindSecret, docstore.ListOptions{})
	if err != nil {
		return out, err
	}
	for _, d := range docs {
		if !internalSecret(d.ID) {
			out[d.ID] = true
		}
	}
	for k := range s.cfg.Secrets {
		out[k] = true
	}
	return out, nil
}

// SecretJSON is what the API says about a secret. There is no value field, on purpose.
type SecretJSON struct {
	Name        string     `json:"name"`
	Source      string     `json:"source"` // "vault" or "environment"
	Description string     `json:"description,omitempty"`
	Version     int        `json:"version,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	// UsedBy lists the saved pipelines whose steps request this secret.
	UsedBy []string `json:"used_by"`
	// Problem is set when the stored value cannot be opened with the current master key.
	Problem string `json:"problem,omitempty"`
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	docs, err := s.docs.List(ctx, kindSecret, docstore.ListOptions{})
	if err != nil {
		s.internalError(w, "listing secrets", err)
		return
	}
	used := s.secretUsage(ctx)
	out := []SecretJSON{}
	seen := map[string]bool{}
	for k := range s.cfg.Secrets {
		seen[k] = true
		out = append(out, SecretJSON{Name: k, Source: "environment", UsedBy: orEmpty(used[k])})
	}
	for _, doc := range docs {
		if internalSecret(doc.ID) || seen[doc.ID] {
			continue
		}
		var d secretDoc
		if err := jsonUnmarshal(doc.Body, &d); err != nil {
			continue
		}
		j := SecretJSON{Name: d.Name, Source: "vault", Description: d.Description, Version: d.Version,
			CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy, UpdatedAt: &d.UpdatedAt, UsedBy: orEmpty(used[d.Name])}
		if _, err := s.vault.Open(d.Name, d.Sealed); err != nil {
			j.Problem = "cannot be opened with the current master key"
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// secretUsage maps a secret name to the saved pipelines that request it.
func (s *Server) secretUsage(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	all, err := s.loadPipelines(ctx)
	if err != nil {
		return out
	}
	for _, sp := range all {
		p, err := pipeline.Load(strings.NewReader(sp.YAML))
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, st := range p.Steps {
			for _, name := range st.Permissions.Secrets {
				if !seen[name] {
					seen[name] = true
					out[name] = append(out[name], sp.ID)
				}
			}
		}
	}
	return out
}

type putSecretRequest struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req putSecretRequest
	if !readJSON(w, r, &req) {
		return
	}
	switch {
	case !secretNameRE.MatchString(name):
		writeError(w, http.StatusBadRequest, "a secret name is letters, digits and underscores, not starting with a digit")
		return
	case len(req.Value) < secrets.MinLength:
		writeError(w, http.StatusBadRequest, "the value must be at least 8 characters so it can be masked in logs")
		return
	case len(req.Value) > maxSecretBytes:
		writeError(w, http.StatusBadRequest, "the value is larger than 16 KiB")
		return
	case strings.ContainsAny(req.Value, "\n\r\x00"):
		writeError(w, http.StatusBadRequest, "the value must not contain a newline or NUL byte")
		return
	case len(req.Description) > 200:
		writeError(w, http.StatusBadRequest, "the description is longer than 200 characters")
		return
	}
	if _, fromEnv := s.cfg.Secrets[name]; fromEnv {
		writeError(w, http.StatusConflict, "this secret is set by the server's environment (FORGELINE_SECRET_"+name+") and cannot be changed here")
		return
	}
	p := principalFrom(r.Context())
	d, rotated, err := s.putSecret(r.Context(), p.Name, name, req.Value, req.Description)
	if err != nil {
		s.internalError(w, "storing the secret", err)
		return
	}
	action := "secret.create"
	if rotated {
		action = "secret.rotate"
	}
	s.auditReq(r, action, "secret/"+name, "version "+itoa(d.Version), true) // never the value
	status := http.StatusCreated
	if rotated {
		status = http.StatusOK
	}
	writeJSON(w, status, SecretJSON{Name: d.Name, Source: "vault", Description: d.Description, Version: d.Version,
		CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy, UpdatedAt: &d.UpdatedAt, UsedBy: []string{}})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if internalSecret(name) {
		writeError(w, http.StatusNotFound, "no such secret")
		return
	}
	if err := s.docs.Delete(r.Context(), kindSecret, name); errors.Is(err, docstore.ErrNotFound) {
		if _, fromEnv := s.cfg.Secrets[name]; fromEnv {
			writeError(w, http.StatusConflict, "this secret is set by the server's environment and cannot be deleted here")
			return
		}
		writeError(w, http.StatusNotFound, "no such secret")
		return
	} else if err != nil {
		s.internalError(w, "deleting the secret", err)
		return
	}
	usedBy := s.secretUsage(r.Context())[name]
	s.auditReq(r, "secret.delete", "secret/"+name, "still requested by: "+strings.Join(usedBy, ", "), true)
	w.WriteHeader(http.StatusNoContent)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func itoa(n int) string { return strconv.Itoa(n) }
