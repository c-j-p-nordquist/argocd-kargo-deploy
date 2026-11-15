// Package cli implements the akd command line.
//
// The CLI is meant to be driven by an agent as well as a person, so every
// command is non-interactive, supports JSON output, and reports failures with
// a stable error code and exit code.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

// Name is the name of the binary. It is a placeholder until a final name is
// chosen, so nothing else in the code base should hard-code it.
const Name = "akd"

// Version is set at build time with -ldflags "-X ...cli.Version=...".
var Version = "dev"

// Exit codes. These are part of the CLI's contract with agents and scripts.
const (
	ExitOK             = 0
	ExitFailure        = 1
	ExitUsage          = 2
	ExitNotImplemented = 3
)

// Error is a failure reported to the caller. Code is stable and safe to
// branch on; Message and Hint are for people.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`

	exit int
}

func (e *Error) Error() string { return e.Message }

type env struct {
	stdout io.Writer
	stderr io.Writer
	format string
}

type command struct {
	name    string
	summary string
	// mutates reports whether the command changes the cluster.
	mutates bool
	run     func(e *env) error
}

func commands() []command {
	return []command{
		{name: "preflight", summary: "Inspect the cluster and report what it supports", run: notImplemented("preflight")},
		{name: "questions", summary: "Print the installation questionnaire", run: runQuestions},
		{name: "validate", summary: "Check platform.yaml against the schema", run: notImplemented("validate")},
		{name: "plan", summary: "Render what would be applied and print its hash", run: notImplemented("plan")},
		{name: "apply", summary: "Apply a previously approved plan", mutates: true, run: notImplemented("apply")},
		{name: "verify", summary: "Audit the cluster against the security baseline", run: notImplemented("verify")},
		{name: "version", summary: "Print the version", run: runVersion},
	}
}

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr, format: "text"}

	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	}

	var cmd *command
	for _, c := range commands() {
		if c.name == args[0] {
			cmd = &c
			break
		}
	}
	if cmd == nil {
		return e.fail(&Error{
			Code:    "unknown_command",
			Message: fmt.Sprintf("unknown command %q", args[0]),
			Hint:    fmt.Sprintf("run `%s help` for the list of commands", Name),
			exit:    ExitUsage,
		})
	}

	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("output", "text", "output format: text or json")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stdout, "%s %s: %s\n\nFlags:\n  --output text|json   output format (default text)\n", Name, cmd.name, cmd.summary)
			return ExitOK
		}
		return e.fail(usageError(err.Error(), cmd.name))
	}
	if *format != "text" && *format != "json" {
		return e.fail(usageError(fmt.Sprintf("invalid --output %q: must be text or json", *format), cmd.name))
	}
	e.format = *format
	if fs.NArg() > 0 {
		return e.fail(usageError(fmt.Sprintf("unexpected argument %q", fs.Arg(0)), cmd.name))
	}

	if err := cmd.run(e); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "%s installs and verifies a hardened Argo CD and Kargo platform.\n\n", Name)
	fmt.Fprintf(w, "Usage: %s <command> [--output text|json]\n\nCommands:\n", Name)
	for _, c := range commands() {
		note := ""
		if c.mutates {
			note = " (changes the cluster)"
		}
		fmt.Fprintf(w, "  %-10s %s%s\n", c.name, c.summary, note)
	}
}

func usageError(msg, cmd string) *Error {
	return &Error{
		Code:    "usage",
		Message: msg,
		Hint:    fmt.Sprintf("run `%s %s --help`", Name, cmd),
		exit:    ExitUsage,
	}
}

func notImplemented(name string) func(*env) error {
	return func(*env) error {
		return &Error{
			Code:    "not_implemented",
			Message: fmt.Sprintf("%s is not implemented yet", name),
			Hint:    "see docs/architecture.md for the planned behaviour",
			exit:    ExitNotImplemented,
		}
	}
}

// fail reports err on stderr in the selected format and returns its exit code.
func (e *env) fail(err error) int {
	var cliErr *Error
	if !errors.As(err, &cliErr) {
		cliErr = &Error{Code: "internal", Message: err.Error(), exit: ExitFailure}
	}
	if e.format == "json" {
		writeJSON(e.stderr, map[string]*Error{"error": cliErr})
	} else {
		fmt.Fprintf(e.stderr, "error: %s\n", cliErr.Message)
		if cliErr.Hint != "" {
			fmt.Fprintf(e.stderr, "hint: %s\n", cliErr.Hint)
		}
	}
	return cliErr.exit
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runVersion(e *env) error {
	if e.format == "json" {
		return writeJSON(e.stdout, map[string]string{"name": Name, "version": Version})
	}
	_, err := fmt.Fprintf(e.stdout, "%s %s\n", Name, Version)
	return err
}

func runQuestions(e *env) error {
	q := questions.Default()
	if e.format == "json" {
		return writeJSON(e.stdout, q)
	}
	for i, question := range q.Questions {
		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		fmt.Fprintf(e.stdout, "%d. %s  [%s -> %s]\n", i+1, question.Prompt, question.ID, question.Field)
		if question.Help != "" {
			fmt.Fprintf(e.stdout, "   %s\n", question.Help)
		}
		for _, o := range question.Options {
			marks := ""
			if o.Value == question.Default {
				marks += " (default)"
			}
			if o.RequiresAcknowledgement {
				marks += " (requires acknowledgement)"
			}
			fmt.Fprintf(e.stdout, "   - %s%s: %s\n     %s\n", o.Value, marks, o.Label, o.Security)
		}
	}
	return nil
}
