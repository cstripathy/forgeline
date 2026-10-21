// Command forgeline is the command-line entry point for Forgeline.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/pipeline"
	"github.com/cstripathy/forgeline/internal/version"
)

const usage = `Forgeline: an open-source delivery platform where AI agents run safely.

Status: pre-alpha. Only the commands below exist.

Usage:
  forgeline version    print the version
  forgeline schema     print the pipeline JSON Schema
  forgeline run [--parallel N] [--no-cache] <pipeline.yaml>
                       run a pipeline in Docker, skipping steps whose inputs are unchanged
  forgeline help       show this message
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, stdout, stderr, executor.Docker{})
}

// runWith is run with the step executor supplied, so tests can avoid Docker.
func runWith(args []string, stdout, stderr io.Writer, ex executor.Executor) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "schema":
		out, err := pipeline.Schema()
		if err != nil {
			fmt.Fprintf(stderr, "forgeline: generating schema: %v\n", err)
			return 1
		}
		stdout.Write(out)
		return 0
	case "run":
		return runPipeline(args[1:], stdout, stderr, ex)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "forgeline: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
