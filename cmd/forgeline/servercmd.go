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
	"syscall"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/server"
	"github.com/cstripathy/forgeline/internal/store"
)

const serverHelp = `usage: forgeline server [--listen ADDR] [--data-dir DIR] [--max-runs N] [--parallel N]

Environment:
  FORGELINE_TOKEN         bearer token clients must send (at least 16 characters). If unset and the
                          listen address is loopback, a random token is generated for this run and
                          printed; on any other address the token is required.
  FORGELINE_DATABASE_URL  Postgres connection URL; if unset, runs are kept in memory and lost on exit

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

	var st store.Store
	if url := getenv("FORGELINE_DATABASE_URL"); url != "" {
		pg, err := store.OpenPostgres(ctx, url)
		if err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
		st = pg
	} else {
		fmt.Fprintln(stderr, "forgeline: FORGELINE_DATABASE_URL is not set; keeping runs in memory (lost on exit)")
		st = store.NewMemory()
	}
	defer st.Close()

	srv, err := server.New(server.Config{
		Store: st, Executor: ex, Token: token, DataDir: *dataDir,
		MaxRuns: *maxRuns, Parallelism: *parallel,
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
