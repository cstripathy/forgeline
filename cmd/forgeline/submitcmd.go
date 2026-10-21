package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cstripathy/forgeline/internal/client"
	"github.com/cstripathy/forgeline/internal/server"
)

const submitHelp = `usage: forgeline submit [--server URL] [--source DIR | --no-source] [--wait] <pipeline.yaml>

Sends a pipeline to a Forgeline server. The workspace is uploaded from the pipeline file's
directory unless --source or --no-source is given.

Environment:
  FORGELINE_TOKEN   bearer token (required)
  FORGELINE_SERVER  server URL, if --server is not given
`

// runSubmit implements `forgeline submit`.
func runSubmit(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, submitHelp) }
	serverURL := fs.String("server", getenv("FORGELINE_SERVER"), "server URL")
	source := fs.String("source", "", "directory to upload as the workspace")
	noSource := fs.Bool("no-source", false, "upload no workspace")
	wait := fs.Bool("wait", false, "wait for the run to finish and print step logs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	token := getenv("FORGELINE_TOKEN")
	if *serverURL == "" || token == "" {
		fmt.Fprintln(stderr, "forgeline: set --server (or FORGELINE_SERVER) and FORGELINE_TOKEN")
		return 2
	}
	file := fs.Arg(0)
	yamlText, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	var archive []byte
	if !*noSource {
		dir := *source
		if dir == "" {
			dir = filepath.Dir(file)
		}
		if archive, err = server.PackDir(dir); err != nil {
			fmt.Fprintf(stderr, "forgeline: packing %s: %v\n", dir, err)
			return 1
		}
	}

	c := client.New(*serverURL, token)
	run, err := c.Submit(ctx, yamlText, archive)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run %s %s\n  %s/v1/runs/%s\n", run.ID, run.Status, c.BaseURL, run.ID)
	if !*wait {
		return 0
	}

	for !client.Terminal(run.Status) {
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "forgeline: interrupted; run %s keeps going on the server\n", run.ID)
			return 1
		case <-time.After(500 * time.Millisecond):
		}
		if run, err = c.Get(ctx, run.ID); err != nil {
			fmt.Fprintf(stderr, "forgeline: %v\n", err)
			return 1
		}
	}
	for _, s := range run.Steps {
		if log, err := c.Logs(ctx, run.ID, s.ID); err == nil && log != "" {
			newPrefixLogs(stdout).printBlock(s.ID, log)
		}
	}
	fmt.Fprintf(stdout, "\nrun %s %s", run.ID, run.Status)
	if run.Reason != "" {
		fmt.Fprintf(stdout, ": %s", run.Reason)
	}
	fmt.Fprintln(stdout)
	for _, s := range run.Steps {
		line := fmt.Sprintf("  %-9s %s", s.Status, s.ID)
		if s.Error != "" {
			line += ": " + s.Error
		}
		fmt.Fprintln(stdout, line)
	}
	if run.Status != "succeeded" {
		return 1
	}
	return 0
}
