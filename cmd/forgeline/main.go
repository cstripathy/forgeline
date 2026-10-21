// Command forgeline is the command-line entry point for Forgeline.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cstripathy/forgeline/internal/version"
)

const usage = `Forgeline: an open-source delivery platform where AI agents run safely.

Status: pre-alpha. Only the commands below exist.

Usage:
  forgeline version    print the version
  forgeline help       show this message
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "forgeline: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
