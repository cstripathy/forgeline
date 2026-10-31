package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/store"
)

// maxPipelineBytes caps the pipeline YAML in a submission.
const maxPipelineBytes = 1 << 20

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	index, assets := uiHandler()
	mux.Handle("GET /{$}", index)
	mux.Handle("GET /ui/", assets)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /v1/runs", s.requireRole(authn.Developer, s.handleSubmit))
	mux.Handle("GET /v1/runs", s.requireRole(authn.Viewer, s.handleList))
	mux.Handle("GET /v1/runs/{id}", s.requireRole(authn.Viewer, s.handleGet))
	// Personal API tokens and the MCP endpoint (note 020). /mcp authenticates by itself: only a personal token works there.
	mux.Handle("GET /v1/tokens", s.requireRole(authn.Viewer, s.handleListTokens))
	mux.Handle("POST /v1/tokens", s.requireRole(authn.Viewer, s.handleCreateToken))
	mux.Handle("DELETE /v1/tokens/{id}", s.requireRole(authn.Viewer, s.handleRevokeToken))
	mux.HandleFunc("POST /mcp", s.handleMCP)
	mux.Handle("GET /v1/runs/{id}/failure", s.requireRole(authn.Viewer, s.handleRunFailure))
	mux.Handle("POST /v1/runs/{id}/cancel", s.requireRole(authn.Developer, s.handleCancel))
	mux.Handle("GET /v1/runs/{id}/steps/{step}/logs", s.requireRole(authn.Viewer, s.handleLogs))

	// Accounts and sessions. State, setup and login are open by design; setup needs the server token.
	mux.HandleFunc("GET /v1/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /v1/auth/setup", s.handleSetup)
	mux.HandleFunc("POST /v1/auth/login", s.handleLogin)
	mux.Handle("POST /v1/auth/logout", s.requireRole(authn.Viewer, s.handleLogout))
	mux.Handle("POST /v1/me/password", s.requireRole(authn.Viewer, s.handleChangeOwnPassword))
	mux.Handle("GET /v1/users", s.requireRole(authn.Admin, s.handleListUsers))
	mux.Handle("POST /v1/users", s.requireRole(authn.Admin, s.handleCreateUser))
	mux.Handle("PATCH /v1/users/{id}", s.requireRole(authn.Admin, s.handleUpdateUser))
	mux.Handle("DELETE /v1/users/{id}", s.requireRole(authn.Admin, s.handleDeleteUser))
	mux.Handle("GET /v1/audit", s.requireRole(authn.Admin, s.handleAudit))

	mux.Handle("GET /v1/overview", s.requireRole(authn.Viewer, s.handleOverview))
	mux.Handle("POST /v1/pipelines/validate", s.requireRole(authn.Viewer, s.handleValidatePipeline))
	mux.Handle("GET /v1/pipelines", s.requireRole(authn.Viewer, s.handleListPipelines))
	mux.Handle("POST /v1/pipelines", s.requireRole(authn.Developer, s.handleCreatePipeline))
	mux.Handle("GET /v1/pipelines/{id}", s.requireRole(authn.Viewer, s.handleGetPipeline))
	mux.Handle("PUT /v1/pipelines/{id}", s.requireRole(authn.Developer, s.handleUpdatePipeline))
	mux.Handle("DELETE /v1/pipelines/{id}", s.requireRole(authn.Admin, s.handleDeletePipeline))
	mux.Handle("POST /v1/pipelines/{id}/run", s.requireRole(authn.Developer, s.handleRunPipeline))
	mux.Handle("POST /v1/pipelines/{id}/versions/{version}/restore", s.requireRole(authn.Developer, s.handleRestorePipeline))
	mux.Handle("GET /v1/infra/runtime", s.requireRole(authn.Viewer, s.handleRuntime))
	mux.Handle("GET /v1/infra/aws", s.requireRole(authn.Viewer, s.handleAWS))
	mux.Handle("GET /v1/ai/workflows", s.requireRole(authn.Developer, s.handleListWorkflows))
	mux.Handle("GET /v1/ai/workflows/{id}", s.requireRole(authn.Developer, s.handleGetWorkflow))
	mux.Handle("POST /v1/ai/workflows", s.requireRole(authn.Admin, s.handleCreateWorkflow))
	mux.Handle("POST /v1/ai/workflows/{id}/versions", s.requireRole(authn.Admin, s.handleWorkflowVersion))
	mux.Handle("POST /v1/ai/workflows/{id}/rollback", s.requireRole(authn.Admin, s.handleWorkflowRollback))
	mux.Handle("POST /v1/ai/workflows/{id}/disable", s.requireRole(authn.Admin, s.handleWorkflowEnable(true)))
	mux.Handle("POST /v1/ai/workflows/{id}/enable", s.requireRole(authn.Admin, s.handleWorkflowEnable(false)))
	mux.Handle("POST /v1/ai/workflows/{id}/run", s.requireRole(authn.Developer, s.handleStartWorkflow))
	mux.Handle("GET /v1/ai/workflow-runs", s.requireRole(authn.Developer, s.handleListWorkflowRuns))
	mux.Handle("GET /v1/ai/workflow-runs/{id}", s.requireRole(authn.Developer, s.handleGetWorkflowRun))
	mux.Handle("POST /v1/ai/workflow-runs/{id}/release", s.requireRole(authn.Admin, s.handleReleaseGate))
	mux.Handle("POST /v1/ai/workflow-runs/{id}/cancel", s.requireRole(authn.Developer, s.handleCancelWorkflowRun))
	mux.Handle("GET /v1/envs", s.requireRole(authn.Developer, s.handleListEnvs))
	mux.Handle("GET /v1/envs/templates", s.requireRole(authn.Developer, s.handleEnvTemplates))
	mux.Handle("GET /v1/envs/policy", s.requireRole(authn.Developer, s.handleEnvPolicy))
	mux.Handle("PUT /v1/envs/policy", s.requireRole(authn.Admin, s.handlePutEnvPolicy))
	mux.Handle("POST /v1/envs/estimate", s.requireRole(authn.Developer, s.handleEnvEstimate))
	mux.Handle("POST /v1/envs", s.requireRole(authn.Developer, s.handleRequestEnv))
	mux.Handle("GET /v1/envs/{id}", s.requireRole(authn.Developer, s.handleGetEnv))
	mux.Handle("POST /v1/envs/{id}/extend", s.requireRole(authn.Developer, s.handleExtendEnv))
	mux.Handle("POST /v1/envs/{id}/destroy", s.requireRole(authn.Developer, s.handleDestroyEnv))
	mux.Handle("GET /v1/k8s/clusters", s.requireRole(authn.Viewer, s.handleListK8sClusters))
	mux.Handle("POST /v1/k8s/clusters", s.requireRole(authn.Admin, s.handleAddK8sCluster))
	mux.Handle("DELETE /v1/k8s/clusters/{id}", s.requireRole(authn.Admin, s.handleDeleteK8sCluster))
	mux.Handle("GET /v1/k8s/clusters/{id}/resources", s.requireRole(authn.Viewer, s.handleK8sResources))
	mux.Handle("GET /v1/guard/findings", s.requireRole(authn.Viewer, s.handleGuardFindings))
	mux.Handle("GET /v1/guard/scans", s.requireRole(authn.Viewer, s.handleGuardScans))
	mux.Handle("POST /v1/guard/scan", s.requireRole(authn.Developer, s.handleGuardScan))
	mux.Handle("PUT /v1/guard/settings", s.requireRole(authn.Admin, s.handleGuardSettings))
	mux.Handle("POST /v1/guard/findings/{id}/accept", s.requireRole(authn.Admin, s.handleGuardAccept))
	mux.Handle("DELETE /v1/guard/findings/{id}/accept", s.requireRole(authn.Admin, s.handleGuardUnaccept))
	mux.Handle("GET /v1/ai/tools", s.requireRole(authn.Developer, s.handleListAITools))
	mux.Handle("GET /v1/ai/skills", s.requireRole(authn.Developer, s.handleListSkills))
	mux.Handle("GET /v1/ai/skills/{id}", s.requireRole(authn.Developer, s.handleGetSkill))
	mux.Handle("POST /v1/ai/skills", s.requireRole(authn.Admin, s.handleCreateSkill))
	mux.Handle("POST /v1/ai/skills/{id}/versions", s.requireRole(authn.Admin, s.handleSkillVersion))
	mux.Handle("POST /v1/ai/skills/{id}/rollback", s.requireRole(authn.Admin, s.handleSkillRollback))
	mux.Handle("DELETE /v1/ai/skills/{id}", s.requireRole(authn.Admin, s.handleDeleteSkill))
	mux.Handle("GET /v1/ai/agents", s.requireRole(authn.Developer, s.handleListAgents))
	mux.Handle("GET /v1/ai/agents/{id}", s.requireRole(authn.Developer, s.handleGetAgent))
	mux.Handle("POST /v1/ai/agents", s.requireRole(authn.Admin, s.handleCreateAgent))
	mux.Handle("POST /v1/ai/agents/{id}/versions", s.requireRole(authn.Admin, s.handleAgentVersion))
	mux.Handle("POST /v1/ai/agents/{id}/rollback", s.requireRole(authn.Admin, s.handleAgentRollback))
	mux.Handle("POST /v1/ai/agents/{id}/disable", s.requireRole(authn.Admin, s.handleAgentEnable(true)))
	mux.Handle("POST /v1/ai/agents/{id}/enable", s.requireRole(authn.Admin, s.handleAgentEnable(false)))
	mux.Handle("GET /v1/plugins", s.requireRole(authn.Developer, s.handleGetPlugins))
	mux.Handle("PUT /v1/plugins/settings", s.requireRole(authn.Admin, s.handlePluginSettings))
	mux.Handle("GET /v1/market/packages", s.requireRole(authn.Developer, s.handleMarketList))
	mux.Handle("POST /v1/market/install", s.requireRole(authn.Admin, s.handleMarketInstall))
	mux.Handle("POST /v1/market/refresh", s.requireRole(authn.Admin, s.handleMarketRefresh))
	mux.Handle("DELETE /v1/market/seen/{id}", s.requireRole(authn.Admin, s.handleMarketForget))
	mux.Handle("DELETE /v1/market/install/{id}", s.requireRole(authn.Admin, s.handleMarketUninstall))
	mux.Handle("POST /v1/ai/agents/{id}/chat", s.requireRole(authn.Developer, s.handleAgentChat))
	mux.Handle("GET /v1/aws/accounts", s.requireRole(authn.Viewer, s.handleListAWSAccounts))
	mux.Handle("POST /v1/aws/accounts", s.requireRole(authn.Admin, s.handleAddAWSAccount))
	mux.Handle("PATCH /v1/aws/accounts/{id}", s.requireRole(authn.Admin, s.handleUpdateAWSAccount))
	mux.Handle("DELETE /v1/aws/accounts/{id}", s.requireRole(authn.Admin, s.handleDeleteAWSAccount))
	mux.Handle("GET /v1/infra/aws/resources", s.requireRole(authn.Viewer, s.handleAWSInventory))
	mux.Handle("GET /v1/infra/aws/cost", s.requireRole(authn.Developer, s.handleAWSCost))
	mux.Handle("GET /v1/integrations/aws", s.requireRole(authn.Developer, s.handleGetAWSSettings))
	mux.Handle("PUT /v1/integrations/aws", s.requireRole(authn.Admin, s.handlePutAWSSettings))
	mux.Handle("GET /v1/integrations/aws/sso", s.requireRole(authn.Developer, s.handleSSOStatus))
	mux.Handle("POST /v1/integrations/aws/sso/start", s.requireRole(authn.Admin, s.handleSSOStart))
	mux.Handle("POST /v1/integrations/aws/sso/poll", s.requireRole(authn.Admin, s.handleSSOPoll))
	mux.Handle("GET /v1/integrations/aws/sso/accounts", s.requireRole(authn.Admin, s.handleSSOAccounts))
	mux.Handle("GET /v1/integrations/aws/sso/roles", s.requireRole(authn.Admin, s.handleSSORoles))
	mux.Handle("POST /v1/integrations/aws/sso/connect", s.requireRole(authn.Admin, s.handleSSOConnect))
	mux.Handle("DELETE /v1/integrations/aws/sso", s.requireRole(authn.Admin, s.handleSSODisconnect))
	mux.Handle("GET /v1/cloud/catalog", s.requireRole(authn.Developer, s.handleCatalog))
	mux.Handle("GET /v1/cloud/changes", s.requireRole(authn.Viewer, s.handleListChanges))
	mux.Handle("POST /v1/cloud/changes", s.requireRole(authn.Developer, s.handlePropose))
	mux.Handle("GET /v1/cloud/changes/{id}", s.requireRole(authn.Viewer, s.handleGetChange))
	mux.Handle("POST /v1/cloud/changes/{id}/approve", s.requireRole(authn.Admin, s.handleApprove))
	mux.Handle("POST /v1/cloud/changes/{id}/reject", s.requireRole(authn.Admin, s.handleReject))
	mux.Handle("POST /v1/cloud/changes/{id}/cancel", s.requireRole(authn.Developer, s.handleCancelChange))
	mux.Handle("GET /v1/assistant", s.requireRole(authn.Developer, s.handleGetAssistant))
	mux.Handle("POST /v1/assistant/chat", s.requireRole(authn.Developer, s.handleChat))
	mux.Handle("GET /v1/integrations/aws/mcp", s.requireRole(authn.Admin, s.handleGetAWSMCP))
	mux.Handle("PUT /v1/integrations/aws/mcp", s.requireRole(authn.Admin, s.handlePutAWSMCP))
	mux.Handle("GET /v1/ai/proposals", s.requireRole(authn.Viewer, s.handleListProposals))
	mux.Handle("GET /v1/ai/proposals/{id}", s.requireRole(authn.Viewer, s.handleGetProposal))
	mux.Handle("POST /v1/ai/proposals/{id}/approve", s.requireRole(authn.Admin, s.handleApproveProposal))
	mux.Handle("POST /v1/ai/proposals/{id}/reject", s.requireRole(authn.Admin, s.handleDecideProposal(true)))
	mux.Handle("POST /v1/ai/proposals/{id}/cancel", s.requireRole(authn.Developer, s.handleDecideProposal(false)))
	mux.Handle("PUT /v1/integrations/assistant", s.requireRole(authn.Admin, s.handlePutAssistant))
	mux.Handle("POST /v1/integrations/assistant/test", s.requireRole(authn.Admin, s.handleTestAssistant))
	mux.Handle("GET /v1/actions/repos", s.requireRole(authn.Developer, s.handleActionsRepos))
	mux.Handle("GET /v1/actions/workflows", s.requireRole(authn.Developer, s.handleActionsWorkflows))
	mux.Handle("GET /v1/actions/runs", s.requireRole(authn.Developer, s.handleActionsRuns))
	mux.Handle("GET /v1/actions/runs/{id}", s.requireRole(authn.Developer, s.handleActionsRun))
	mux.Handle("GET /v1/actions/jobs/{id}/logs", s.requireRole(authn.Developer, s.handleActionsLogs))
	mux.Handle("POST /v1/actions/dispatch", s.requireRole(authn.Developer, s.handleActionsDispatch))
	mux.Handle("POST /v1/actions/runs/{id}/cancel", s.requireRole(authn.Developer, s.handleActionsRunAction("cancel")))
	mux.Handle("POST /v1/actions/runs/{id}/rerun", s.requireRole(authn.Developer, s.handleActionsRunAction("rerun")))
	mux.Handle("GET /v1/secrets", s.requireRole(authn.Developer, s.handleListSecrets))
	mux.Handle("PUT /v1/secrets/{name}", s.requireRole(authn.Admin, s.handlePutSecret))
	mux.Handle("DELETE /v1/secrets/{name}", s.requireRole(authn.Admin, s.handleDeleteSecret))
	// No bearer token: GitHub cannot send one. The HMAC signature is the authentication. The
	// route always exists; it answers 404 until the integration is configured.
	mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	mux.Handle("GET /v1/integrations/github", s.requireRole(authn.Developer, s.handleGetGitHub))
	mux.Handle("PUT /v1/integrations/github", s.requireRole(authn.Admin, s.handlePutGitHub))
	mux.Handle("DELETE /v1/integrations/github", s.requireRole(authn.Admin, s.handleDeleteGitHub))
	mux.Handle("POST /v1/integrations/github/app/start", s.requireRole(authn.Admin, s.handleGHAppStart))
	mux.Handle("GET /v1/integrations/github/app/launch", s.requireRole(authn.Admin, s.handleGHAppLaunch))
	mux.HandleFunc("GET /v1/integrations/github/app/callback", s.handleGHAppCallback)
	mux.Handle("GET /v1/integrations/github/app/installations", s.requireRole(authn.Admin, s.handleGHAppInstallations))
	mux.Handle("GET /v1/integrations/github/app/repos", s.requireRole(authn.Admin, s.handleGHAppRepos))
	mux.Handle("POST /v1/integrations/github/app/install", s.requireRole(authn.Admin, s.handleGHAppInstall))
	mux.Handle("POST /v1/integrations/github/test", s.requireRole(authn.Admin, s.handleTestGitHub))
	return mux
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

	var unpack func(ws string) error
	if archive != "" {
		unpack = func(ws string) error {
			f, err := os.Open(archive)
			if err != nil {
				return err
			}
			defer f.Close()
			return ExtractTarGz(f, ws, s.cfg.MaxSourceBytes)
		}
	}
	run, steps, apiErr := s.enqueue(r.Context(), yamlText, store.TriggerAPI, unpack)
	if apiErr != nil {
		apiErr.write(w)
		return
	}
	p := principalFrom(r.Context())
	s.recordRunMeta(r.Context(), run.ID, runMeta{Actor: p.Name})
	s.auditReq(r, "run.start", "run/"+run.ID, run.PipelineName+" via API", true)
	w.Header().Set("Location", "/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, s.decorate(r.Context(), runDetail(run, steps)))
}

// apiError is a failure that maps to an HTTP response.
type apiError struct {
	status int
	body   ErrorJSON
}

func (e *apiError) write(w http.ResponseWriter) { writeJSON(w, e.status, e.body) }

func badRequest(msg string) *apiError {
	return &apiError{http.StatusBadRequest, ErrorJSON{Error: msg}}
}

// validationProblems turns a pipeline load or plan error into an API error body.
func validationProblems(err error) *apiError {
	var ve *pipeline.ValidationError
	if errors.As(err, &ve) {
		body := ErrorJSON{Error: "invalid pipeline"}
		for _, fe := range ve.Errors {
			body.Problems = append(body.Problems, ProblemJSON{Path: fe.Path, Msg: fe.Msg})
		}
		return &apiError{http.StatusBadRequest, body}
	}
	return badRequest(err.Error())
}

// enqueue validates a pipeline, prepares its workspace and queues a run. unpack, if set,
// fills the workspace (an uploaded archive); it runs before any run exists, so a bad archive
// creates nothing.
func (s *Server) enqueue(ctx context.Context, yamlText string, trigger store.Trigger, unpack func(ws string) error) (store.Run, []store.Step, *apiError) {
	internal := func(what string, err error) (store.Run, []store.Step, *apiError) {
		s.log.Error(what, "err", err)
		return store.Run{}, nil, &apiError{http.StatusInternalServerError, ErrorJSON{Error: "internal error: " + what}}
	}
	p, err := pipeline.Load(strings.NewReader(yamlText))
	if err == nil {
		_, err = pipeline.Plan(p)
	}
	if err != nil {
		return store.Run{}, nil, validationProblems(err)
	}

	// Unpack into scratch space first so a bad archive creates no run.
	ws, err := os.MkdirTemp(filepath.Join(s.cfg.DataDir, "tmp"), "ws-")
	if err != nil {
		return internal("preparing workspace", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(ws)
		}
	}()
	if unpack != nil {
		err := unpack(ws)
		if errors.Is(err, ErrBadArchive) {
			return store.Run{}, nil, badRequest(err.Error())
		}
		if err != nil {
			return internal("unpacking source", err)
		}
	}

	stepIDs := make([]string, len(p.Steps))
	for i, st := range p.Steps {
		stepIDs[i] = st.ID
	}
	run, err := s.cfg.Store.CreateRun(ctx, store.Run{PipelineName: p.Name, PipelineYAML: yamlText, Trigger: trigger}, stepIDs)
	if err != nil {
		return internal("creating run", err)
	}
	if err := os.Rename(ws, s.workspace(run.ID)); err != nil {
		s.failQueued(run.ID, "could not place workspace")
		return internal("placing workspace", err)
	}
	committed = true

	select {
	case s.wake <- struct{}{}:
	default:
	}
	got, steps, err := s.cfg.Store.GetRun(ctx, run.ID)
	if err != nil {
		return internal("reading run", err)
	}
	return got, steps, nil
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
	if name := r.URL.Query().Get("pipeline"); name != "" {
		kept := runs[:0:0]
		for _, run := range runs {
			if run.PipelineName == name {
				kept = append(kept, run)
			}
		}
		runs = kept
	}
	out := ListJSON{Runs: make([]RunJSON, len(runs))}
	for i, run := range runs {
		out.Runs[i] = s.decorate(r.Context(), runJSON(run, nil))
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
	writeJSON(w, http.StatusOK, s.decorate(r.Context(), runDetail(run, steps)))
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
			s.reportFinished(run, store.RunCanceled, "canceled by user")
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
	writeJSON(w, status, runDetail(run, steps))
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
