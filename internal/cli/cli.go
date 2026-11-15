// Package cli implements the akd command line.
//
// The CLI is meant to be driven by an agent as well as a person, so every
// command is non-interactive, supports JSON output, and reports failures with
// a stable error code and exit code.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/components"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/kube"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/platform"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/preflight"
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
	// ExitInvalid means the command ran and what it checked did not pass: an
	// invalid file, or a cluster that is not ready. The details are in the
	// command's normal output, not in an error.
	ExitInvalid = 4
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
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	format string
	// file is the path given with --file, for commands that take one.
	file string
	// kubeconfig and kubeContext select the cluster, for commands that
	// connect to one.
	kubeconfig  string
	kubeContext string
}

// exitCode lets a command finish normally with a non-zero exit code.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit code %d", int(c)) }

type command struct {
	name    string
	summary string
	// mutates reports whether the command changes the cluster.
	mutates bool
	// flags registers the command's own flags, if it has any.
	flags func(fs *flag.FlagSet, e *env)
	run   func(e *env) error
}

func commands() []command {
	return []command{
		{name: "preflight", summary: "Inspect the cluster and report what it supports", flags: clusterFlags, run: runPreflight},
		{name: "questions", summary: "Print the installation questionnaire", run: runQuestions},
		{name: "components", summary: "List the pinned third-party components and how each is verified", run: runComponents},
		{name: "schema", summary: "Print the JSON Schema for platform.yaml", run: runSchema},
		{name: "validate", summary: "Check platform.yaml against the questionnaire", flags: fileFlag, run: runValidate},
		{name: "plan", summary: "Render what would be applied and print its hash", run: notImplemented("plan")},
		{name: "apply", summary: "Apply a previously approved plan", mutates: true, run: notImplemented("apply")},
		{name: "verify", summary: "Audit the cluster against the security baseline", run: notImplemented("verify")},
		{name: "version", summary: "Print the version", run: runVersion},
	}
}

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr, format: "text"}

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
	format := fs.String("output", "text", "output format: `text or json`")
	if cmd.flags != nil {
		cmd.flags(fs, e)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stdout, "%s %s: %s\n\nFlags:\n", Name, cmd.name, cmd.summary)
			fs.SetOutput(stdout)
			fs.PrintDefaults()
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
		var code exitCode
		if errors.As(err, &code) {
			return int(code)
		}
		return e.fail(err)
	}
	return ExitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "%s installs and verifies a hardened Argo CD and Kargo platform.\n\n", Name)
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nEvery command accepts --output text|json. Run `%s <command> --help` for its flags.\n\nCommands:\n", Name, Name)
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

func runSchema(e *env) error {
	return writeJSON(e.stdout, platform.Schema(questions.Default()))
}

func fileFlag(fs *flag.FlagSet, e *env) {
	fs.StringVar(&e.file, "file", "platform.yaml", "`path` to the answers file, or - for standard input")
}

func runValidate(e *env) error {
	var (
		data []byte
		err  error
	)
	if e.file == "-" {
		data, err = io.ReadAll(e.stdin)
	} else {
		data, err = os.ReadFile(e.file)
	}
	if err != nil {
		code, hint := "file_unreadable", ""
		if errors.Is(err, os.ErrNotExist) {
			code = "file_not_found"
			hint = fmt.Sprintf("pass the path with --file, or answer the questions from `%s questions` first", Name)
		}
		return &Error{Code: code, Message: err.Error(), Hint: hint, exit: ExitFailure}
	}

	res, err := platform.Validate(data, questions.Default())
	if err != nil {
		return &Error{Code: "invalid_yaml", Message: err.Error(), exit: ExitFailure}
	}

	if e.format == "json" {
		if err := writeJSON(e.stdout, struct {
			File string `json:"file"`
			platform.Result
		}{e.file, res}); err != nil {
			return err
		}
	} else {
		name := e.file
		if name == "-" {
			name = "standard input"
		}
		printValidation(e.stdout, name, res)
	}
	if !res.Valid {
		return exitCode(ExitInvalid)
	}
	return nil
}

func printValidation(w io.Writer, file string, res platform.Result) {
	if !res.Valid {
		fmt.Fprintf(w, "%s is not valid: %d problem(s)\n", file, len(res.Problems))
		for _, p := range res.Problems {
			fmt.Fprintf(w, "\n  %s: %s  [%s]\n", p.Field, p.Message, p.Code)
			if p.Hint != "" {
				fmt.Fprintf(w, "    hint: %s\n", p.Hint)
			}
		}
		return
	}
	fmt.Fprintf(w, "%s is valid\n", file)
	if len(res.Deviations) == 0 {
		fmt.Fprintln(w, "No departures from the secure defaults.")
		return
	}
	fmt.Fprintf(w, "\nAcknowledged departures from the secure defaults: %d\n", len(res.Deviations))
	for _, d := range res.Deviations {
		fmt.Fprintf(w, "\n  %s = %s\n    consequence: %s\n    reason given: %s\n", d.Field, d.Option, d.Security, d.Reason)
	}
}

func clusterFlags(fs *flag.FlagSet, e *env) {
	fs.StringVar(&e.kubeconfig, "kubeconfig", "", "`path` to the kubeconfig (default: $KUBECONFIG, then ~/.kube/config)")
	fs.StringVar(&e.kubeContext, "context", "", "kubeconfig context `name` (default: the current context)")
}

func runPreflight(e *env) error {
	client, err := kube.Connect(e.kubeconfig, e.kubeContext)
	if err != nil {
		return &Error{
			Code:    "kubeconfig_invalid",
			Message: "could not load the kubeconfig: " + err.Error(),
			Hint:    "pass --kubeconfig and --context, or set KUBECONFIG",
			exit:    ExitFailure,
		}
	}
	report, err := preflight.Run(context.Background(), client, client.Context)
	if err != nil {
		return &Error{
			Code:    "cluster_unreachable",
			Message: "could not reach the cluster: " + err.Error(),
			Hint:    "check that the cluster is running and the kubeconfig credentials are current",
			exit:    ExitFailure,
		}
	}

	if e.format == "json" {
		if err := writeJSON(e.stdout, report); err != nil {
			return err
		}
	} else {
		printPreflight(e.stdout, report)
	}
	if !report.Ready {
		return exitCode(ExitInvalid)
	}
	return nil
}

func printPreflight(w io.Writer, r preflight.Report) {
	nodes := "unknown number of nodes"
	if r.Cluster.Nodes >= 0 {
		nodes = fmt.Sprintf("%d node(s)", r.Cluster.Nodes)
	}
	fmt.Fprintf(w, "Cluster: context %q, Kubernetes %s, distro %s, %s\n", r.Cluster.Context, r.Cluster.KubernetesVersion, r.Cluster.Distro, nodes)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "\n  %-7s %s  [%s]\n          %s\n", strings.ToUpper(string(c.Status)), c.Title, c.ID, c.Message)
		if c.Hint != "" {
			fmt.Fprintf(w, "          hint: %s\n", c.Hint)
		}
	}
	for field, value := range r.Suggestions {
		fmt.Fprintf(w, "\nSuggested answer: %s = %s\n", field, value)
	}
	if r.Ready {
		fmt.Fprintln(w, "\nNo check failed. Review the warnings before continuing.")
	} else {
		fmt.Fprintln(w, "\nAt least one check failed. Fix it before continuing.")
	}
}

func runComponents(e *env) error {
	lock, err := components.LoadLock()
	if err != nil {
		return err
	}
	if e.format == "json" {
		return writeJSON(e.stdout, lock)
	}

	images, unverified := 0, 0
	for i, c := range lock.Components {
		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		kind := "required"
		if c.Optional {
			kind = "optional"
		}
		fmt.Fprintf(e.stdout, "%s  chart %s  (%s)\n  %s\n", c.Name, c.Chart.Version, kind, c.Purpose)
		for _, img := range c.Images {
			images++
			if img.Verification.Method == components.MethodNone {
				unverified++
			}
			fmt.Fprintf(e.stdout, "    %-18s %s:%s\n", img.Verification.Method, img.Repository, img.Tag)
		}
	}
	fmt.Fprintf(e.stdout, "\n%d components, %d images, all pinned by digest. %d verified, %d pinned by digest only.\n",
		len(lock.Components), images, images-unverified, unverified)
	fmt.Fprintln(e.stdout, "Charts are pinned by digest but not signature-checked.")
	return nil
}
