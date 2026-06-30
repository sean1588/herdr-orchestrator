// Command orchestratord is the Herdr Orchestrator control-plane daemon and CLI.
//
// Phase 1 subcommands:
//
//	orchestratord validate <config.yaml>
//	    Validate a workflow config (JSON Schema + safety invariants).
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return cmdValidate(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: orchestratord <command> [args]

commands:
  validate <config.yaml>   validate a workflow config (schema + safety invariants)
`)
}

// cmdValidate validates a workflow config, printing warnings and errors in the
// same shape as the reference validator. Exit 0 if valid (warnings allowed),
// 1 on validation failure, 2 on usage/IO error.
func cmdValidate(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: orchestratord validate <config.yaml>")
		return 2
	}

	wf, warnings, err := config.Load(args[0])
	for _, w := range warnings {
		fmt.Printf("  WARN  %s\n", w)
	}

	var ve *config.ValidationErrors
	if errors.As(err, &ve) {
		for _, e := range ve.Errors {
			fmt.Printf("  ERROR %s\n", e)
		}
		fmt.Printf("\nFAIL: %d error(s), %d warning(s)\n", len(ve.Errors), len(warnings))
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	fmt.Printf("\nOK: %q valid (%d warning(s))\n", wf.Name, len(warnings))
	return 0
}
