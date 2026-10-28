package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cstripathy/forgeline/internal/docstore"
	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/secrets"
	"github.com/cstripathy/forgeline/internal/server"
	"github.com/cstripathy/forgeline/internal/store"
	"github.com/cstripathy/forgeline/internal/vault"
)

const serverHelp = `usage: forgeline server [--listen ADDR] [--data-dir DIR] [--max-runs N] [--parallel N]
                        [--public-url URL] [--github-allow OWNER/REPO:BRANCH[,BRANCH...]]...

Environment:
  FORGELINE_TOKEN         bearer token clients must send (at least 16 characters). If unset and the
                          listen address is loopback, a random token is generated for this run and
                          printed; on any other address the token is required.
  FORGELINE_DATABASE_URL  Postgres connection URL; if unset, runs are kept in memory and lost on exit.
                          Accounts, pipelines, secrets and settings are stored in the same database, or
                          in <data-dir>/console.json when there is none
  FORGELINE_MASTER_KEY    32 random bytes as base64 (head -c 32 /dev/urandom | base64) that encrypt
                          secrets stored by the console. If unset, a key file is created in the data dir
  FORGELINE_SECRET_<NAME> a secret that steps can request with permissions.secrets: [NAME]. Values
                          are at least 8 characters, never stored, and masked in logs. Runs started
                          by GitHub receive no secrets yet.

GitHub triggers (POST /webhooks/github) turn on when --github-allow is given. They also need:
  FORGELINE_GITHUB_WEBHOOK_SECRET  the webhook's secret (at least 16 characters)
  FORGELINE_GITHUB_TOKEN           a fine-grained token: contents read, commit statuses write
  FORGELINE_GITHUB_API_URL         API root for GitHub Enterprise Server (default https://api.github.com)
Only the repositories and branches in --github-allow can start runs, and pull requests from
forks never do. A branch may be a pattern such as release/* (the * stops at a slash). The
repository needs a forgeline.yaml at its root. --public-url is the address people use to reach
this server; it becomes the link on commit statuses. GitHub must be able to reach the server,
so put it behind TLS (a reverse proxy or a tunnel); the server itself speaks plain HTTP.

M2 has no sandbox beyond container defaults. Anyone with the token can run containers on this
host. The default listen address is loopback only; the server speaks plain HTTP, so put it
behind TLS before exposing it.
`

// runServer implements `forgeline server`. It blocks until ctx ends.
func runServer(ctx context.Context, args []string, stdout, stderr io.Writer, ex executor.Executor, getenv func(string) string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, serverHelp) }
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	dataDir := fs.String("data-dir", "forgeline-data", "directory for run workspaces")
	maxRuns := fs.Int("max-runs", 2, "runs to execute at once")
	parallel := fs.Int("parallel", 4, "steps to execute at once within a run")
	publicURL := fs.String("public-url", "", "address users reach this server at, for links on commit statuses")
	var allow allowFlag
	fs.Var(&allow, "github-allow", "repository and branches allowed to trigger runs, as owner/repo:branch[,branch]; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	token := getenv("FORGELINE_TOKEN")
	generated := false
	if token == "" {
		if !isLoopbackListen(*listen) {
			fmt.Fprintf(stderr, "forgeline: FORGELINE_TOKEN is required when listening on %s (a token is only generated for loopback addresses)\n", *listen)
			return 1
		}
		var err error
		if token, err = randomToken(); err != nil {
			fmt.Fprintf(stderr, "forgeline: generating a token: %v\n", err)
			return 1
		}
		generated = true
	}

	stepSecrets, err := secrets.FromEnviron(environ())
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}

	var st store.Store
	var docs docstore.Store
	storeKind := "memory"
	if url := getenv("FORGELINE_DATABASE_URL"); url != "" {
		pg, err := store.OpenPostgres(ctx, url)
		if err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
		st = pg
		pgDocs, err := docstore.OpenPostgres(ctx, url)
		if err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
		docs = pgDocs
		storeKind = "postgres"
	} else {
		fmt.Fprintln(stderr, "forgeline: FORGELINE_DATABASE_URL is not set; keeping runs in memory (lost on exit)")
		st = store.NewMemory()
		if err := os.MkdirAll(*dataDir, 0o700); err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
		fileDocs, err := docstore.OpenFile(filepath.Join(*dataDir, "console.json"))
		if err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
		docs = fileDocs
		fmt.Fprintf(stderr, "forgeline: accounts, pipelines and settings are kept in %s\n", filepath.Join(*dataDir, "console.json"))
	}
	defer st.Close()
	defer docs.Close()

	keyBytes, keyCreated, err := vault.LoadOrCreateKey(getenv("FORGELINE_MASTER_KEY"), filepath.Join(*dataDir, "master.key"))
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: master key: %v\n", err)
		return 1
	}
	keySource := "file"
	if getenv("FORGELINE_MASTER_KEY") != "" {
		keySource = "env"
	} else if keyCreated {
		fmt.Fprintf(stderr, "forgeline: created a master key at %s to encrypt stored secrets. It sits next to the data it protects; for a real deployment set FORGELINE_MASTER_KEY from a secret manager instead.\n", filepath.Join(*dataDir, "master.key"))
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: master key: %v\n", err)
		return 1
	}

	var gh *server.GitHubConfig
	secret, ghToken := getenv("FORGELINE_GITHUB_WEBHOOK_SECRET"), getenv("FORGELINE_GITHUB_TOKEN")
	switch {
	case len(allow.rules) > 0:
		gh = &server.GitHubConfig{WebhookSecret: secret, Token: ghToken, Allow: allow.rules, PublicURL: *publicURL,
			APIBase: getenv("FORGELINE_GITHUB_API_URL")}
	case secret != "" || ghToken != "":
		fmt.Fprintln(stderr, "forgeline: FORGELINE_GITHUB_* is set but --github-allow is not; list the repositories and branches that may trigger runs")
		return 2
	}

	srv, err := server.New(server.Config{
		Store: st, Executor: ex, Token: token, DataDir: *dataDir,
		MaxRuns: *maxRuns, Parallelism: *parallel, GitHub: gh, Secrets: stepSecrets,
		Docs: docs, Vault: vlt, StoreKind: storeKind, KeySource: keySource,
		Logger: slog.New(slog.NewTextHandler(stderr, nil)),
	})
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	defer srv.Stop()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	httpSrv := srv.HTTPServer(*listen)
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	fmt.Fprintf(stdout, "listening on http://%s\n", ln.Addr())
	if len(stepSecrets) > 0 {
		fmt.Fprintf(stdout, "secrets available to steps that list them: %s\n", strings.Join(secrets.Names(stepSecrets), ", "))
	}
	if gh != nil {
		fmt.Fprintf(stdout, "GitHub webhook: POST /webhooks/github, %d repository rule(s)\n", len(gh.Allow))
	}
	if generated {
		// The token sits in the URL fragment, which browsers never send to the server.
		fmt.Fprintf(stdout, "\nNo FORGELINE_TOKEN was set, so a token was generated for this run only.\n"+
			"  Open:    http://%s/#token=%s\n  CLI:     export FORGELINE_TOKEN=%s FORGELINE_SERVER=http://%s\n\n",
			ln.Addr(), token, token, ln.Addr())
	}

	select {
	case err := <-errc:
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "forgeline: shutting down: %v\n", err)
	}
	return 0
}

// allowFlag collects repeated --github-allow values.
type allowFlag struct{ rules []server.RepoRule }

func (a *allowFlag) String() string { return fmt.Sprint(a.rules) }

func (a *allowFlag) Set(v string) error {
	repo, branches, ok := strings.Cut(v, ":")
	if !ok || repo == "" || branches == "" {
		return fmt.Errorf("%q: want owner/repo:branch[,branch...]", v)
	}
	a.rules = append(a.rules, server.RepoRule{Repo: repo, Branches: strings.Split(branches, ",")})
	return nil
}

// isLoopbackListen reports whether addr only accepts connections from this machine.
// An empty host (":8080") listens on every interface, so it is not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// randomToken returns 32 random bytes as URL-safe text.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// signalContext is cancelled on SIGINT or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
