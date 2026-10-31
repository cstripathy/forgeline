// Package server is the Forgeline control plane: an HTTP API over a Store, plus an embedded
// runner that executes queued runs. See docs/design/003-control-plane.md.
//
// M2 has no sandbox beyond container defaults and one shared bearer token. Anyone holding the
// token can run arbitrary containers on the host, so do not expose the server to people you
// would not give a shell.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
	"github.com/forgelinehq/forgeline/internal/executor"
	"github.com/forgelinehq/forgeline/internal/store"
	"github.com/forgelinehq/forgeline/internal/vault"
)

// MinTokenLength is the shortest bearer token the server accepts.
const MinTokenLength = 16

// Config configures a Server. Store, Executor, Token and DataDir are required.
type Config struct {
	Store    store.Store
	Executor executor.Executor
	// Token is the bearer token clients must send.
	Token string
	// DataDir holds per-run workspaces under runs/ and upload scratch space under tmp/.
	DataDir string
	// MaxRuns is how many runs execute at once. Zero means 2.
	MaxRuns int
	// Parallelism is how many steps of one run execute at once. Zero means 4.
	Parallelism int
	// MaxSourceBytes caps an uploaded source archive, compressed and unpacked. Zero means 64 MiB.
	MaxSourceBytes int64
	// MaxLogBytes caps the stored log of one step. Zero means 10 MiB.
	MaxLogBytes int64
	// LogPollInterval is how often a live log stream checks the store for new output.
	// Zero means 250 ms.
	LogPollInterval time.Duration
	// Secrets are the values steps may request through permissions.secrets, by name. They
	// come from the server's own configuration, never from a pipeline or an API request.
	Secrets map[string]string
	// GitHub, if set, enables POST /webhooks/github and commit status reporting.
	GitHub *GitHubConfig
	// StoreKind and KeySource are shown on the infrastructure page: "memory" or "postgres" for
	// the run store, and "env", "file" or "ephemeral" for where the vault key came from.
	StoreKind string
	KeySource string
	// Docs keeps the console's accounts, pipelines, secrets and audit log. Nil means an
	// in-memory store, which loses everything on exit.
	Docs docstore.Store
	// Vault encrypts console secrets. Nil means a random key for this process only.
	Vault  *vault.Vault
	Logger *slog.Logger
	// SSOBase replaces the AWS sign-in addresses with <base>/oidc and <base>/portal. It exists for
	// development and tests against a stand-in, and must be a loopback address.
	SSOBase string
}

// Server serves the API and runs queued pipelines.
type Server struct {
	cfg Config
	log *slog.Logger

	wake chan struct{}

	mu            sync.Mutex
	ctls          map[string]*runCtl
	pendingCancel map[string]bool

	cancel  context.CancelFunc
	baseCtx context.Context // ends when the server stops; set by Start
	wg      sync.WaitGroup

	deliveries deliveryLog
	checkouts  chan struct{} // bounds GitHub downloads in flight

	docs     docstore.Store
	vault    *vault.Vault
	throttle throttle
	setupMu  sync.Mutex                                                 // serialises first-run setup
	userMu   sync.Mutex                                                 // serialises changes to accounts, for the last-admin rule
	pipeMu   sync.Mutex                                                 // serialises pipeline edits, for the version check
	aiMu     sync.Mutex                                                 // guards aiBusy and proposal decisions
	aiBusy   bool                                                       // an approved AI change is being applied
	mcpStart func(ctx context.Context, st awsSettings) (mcpConn, error) // replaced in tests

	started  time.Time
	run      cmdRunner // runs docker and aws; replaced in tests
	awsCache awsCache
	awsInv   invCache
	wf       wfEngine
	guard    guardEngine
	k8s      k8sEngine
	envs     envEngine
	plugins  pluginRunner // container runner for plugins; replaced in tests

	ghApp     ghAppRegistry // GitHub App setups waiting for GitHub
	ghTokens  sync.Map      // "appID/installationID" -> *ghTokenCache
	ghAppForm sync.Map      // state -> ghAppForm, what the launch page shows

	sso ssoState // AWS SSO sign-ins in progress and the cached role credentials

	assistLimiter  rateLimiter // questions per person per minute
	actionsLimiter rateLimiter // GitHub Actions writes per person per minute
	cloud          cloudState

	ghCfg        atomic.Pointer[GitHubConfig] // the active GitHub settings; nil means off
	ghSource     atomic.Value                 // "flags" or "console"
	ghMu         sync.Mutex                   // serialises changes to the GitHub settings
	deliveryRing deliveryRing
}

// New validates cfg, prepares the data directory and returns a Server. Call Start to begin
// executing runs, and Handler to serve the API.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("server: Store is required")
	case cfg.Executor == nil:
		return nil, errors.New("server: Executor is required")
	case len(cfg.Token) < MinTokenLength:
		return nil, fmt.Errorf("server: token must be at least %d characters", MinTokenLength)
	case cfg.DataDir == "":
		return nil, errors.New("server: DataDir is required")
	}
	if cfg.MaxRuns <= 0 {
		cfg.MaxRuns = 2
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 4
	}
	if cfg.MaxSourceBytes <= 0 {
		cfg.MaxSourceBytes = 64 << 20
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 10 << 20
	}
	if cfg.LogPollInterval <= 0 {
		cfg.LogPollInterval = 250 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
	if cfg.GitHub != nil {
		gh := *cfg.GitHub // validation fills defaults; do not change the caller's copy
		if err := validateGitHub(&gh); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		cfg.GitHub = &gh
	}
	for _, d := range []string{"runs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return nil, fmt.Errorf("server: preparing data dir: %w", err)
		}
	}
	if cfg.Docs == nil {
		cfg.Docs = docstore.NewMemory()
	}
	if cfg.Vault == nil {
		key := make([]byte, vault.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		v, err := vault.New(key)
		if err != nil {
			return nil, err
		}
		cfg.Vault = v
	}
	srv := &Server{
		started: time.Now().UTC(), run: execRunner,
		docs: cfg.Docs, vault: cfg.Vault,
		cfg: cfg, log: cfg.Logger,
		wake:          make(chan struct{}, cfg.MaxRuns),
		ctls:          map[string]*runCtl{},
		pendingCancel: map[string]bool{},
	}
	maxCheckouts := 4
	if cfg.GitHub != nil {
		maxCheckouts = cfg.GitHub.MaxCheckouts
		srv.ghCfg.Store(cfg.GitHub)
		srv.ghSource.Store("flags")
	}
	srv.checkouts = make(chan struct{}, maxCheckouts)
	if err := srv.reloadGitHub(context.Background()); err != nil {
		srv.log.Warn("the saved GitHub settings are not usable; GitHub triggers are off", "err", err)
	}
	return srv, nil
}

// Start marks runs interrupted by a previous crash as failed, then starts the workers. It
// returns once the workers are running. Stop waits for them.
func (s *Server) Start(ctx context.Context) error {
	// Commit statuses of interrupted GitHub runs would stay "pending" forever, so note which
	// runs are about to be failed before failing them.
	var interrupted []store.Run
	if s.gh() != nil {
		recent, err := s.cfg.Store.ListRuns(ctx, store.ListOptions{Limit: 200})
		if err != nil {
			return fmt.Errorf("recovering interrupted runs: %w", err)
		}
		for _, r := range recent {
			if r.Status == store.RunRunning && r.Trigger == store.TriggerGitHub {
				interrupted = append(interrupted, r)
			}
		}
	}
	s.failInterruptedChanges(ctx)
	n, err := s.cfg.Store.FailInterruptedRuns(ctx, "server restarted")
	if err != nil {
		return fmt.Errorf("recovering interrupted runs: %w", err)
	}
	if n > 0 {
		s.log.Warn("marked interrupted runs failed", "count", n)
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.baseCtx = ctx
	for _, r := range interrupted {
		s.reportFinished(r, store.RunFailed, "server restarted")
	}
	s.startWorkflows(ctx)
	for i := 0; i < s.cfg.MaxRuns; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.worker(ctx)
		}()
	}
	return nil
}

// Stop cancels running work and waits for the workers to finish recording results.
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// HTTPServer returns an http.Server with conservative timeouts for the given address.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}
