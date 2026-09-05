// Command ticketprep is #222's developer-only, offline ticket preflight
// kernel. It never contacts a forge, invokes a model, edits an issue, or
// participates in BEN's daemon authority loop.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/srhg-ai-7cef3f93/ben/internal/ticketprep"
)

const usage = `ticketprep — bounded, read-only ticket preflight (#222)

Usage:
  ticketprep capture   [-repo path] -issue issue.json [-out capture.json]
  ticketprep validate  -capture capture.json -advice advice.json [-out packet.json]
  ticketprep freshness -packet packet.json -current capture.json [-out freshness.json]
  ticketprep render    -packet packet.json -current capture.json [-dispositions dispositions.json] [-out report.md]

Use - for one input or for stdout. The command performs no network or model
invocation and no tracker, documentation, label, queue, or dispatch write.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "capture":
		err = runCapture(ctx, args[1:], stdin, stdout, stderr)
	case "validate":
		err = runValidate(args[1:], stdin, stdout, stderr)
	case "freshness":
		err = runFreshness(args[1:], stdin, stdout, stderr)
	case "render":
		err = runRender(args[1:], stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "ticketprep: unknown operation %q\n\n%s", args[0], usage)
		return 2
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errUsage) {
		fmt.Fprintf(stderr, "ticketprep: %v\n", errors.Unwrap(err))
		return 2
	}
	fmt.Fprintf(stderr, "ticketprep: %v\n", err)
	return 1
}

var errUsage = errors.New("usage")

func runCapture(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("capture", stderr)
	repo := fs.String("repo", ".", "repository worktree used to locate committed Git objects")
	issuePath := fs.String("issue", "", "strict issue snapshot JSON, or - for stdin")
	out := fs.String("out", "-", "capture output path, or - for stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *issuePath == "" {
		return usageError("capture requires -issue")
	}
	issueFile, closeFile, err := input(*issuePath, stdin)
	if err != nil {
		return err
	}
	defer closeFile()
	issue, err := ticketprep.DecodeIssue(issueFile)
	if err != nil {
		return err
	}
	capture, err := ticketprep.CaptureIssue(ctx, *repo, issue)
	if err != nil {
		return err
	}
	return encodeOutput(*out, stdout, func(w io.Writer) error { return ticketprep.Encode(w, capture) })
}

func runValidate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("validate", stderr)
	capturePath := fs.String("capture", "", "capture artifact")
	advicePath := fs.String("advice", "", "strict advisory JSON")
	out := fs.String("out", "-", "packet output path, or - for stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *capturePath == "" || *advicePath == "" {
		return usageError("validate requires -capture and -advice")
	}
	if *capturePath == "-" && *advicePath == "-" {
		return usageError("only one validate input may use stdin")
	}
	capture, err := readCapture(*capturePath, stdin)
	if err != nil {
		return err
	}
	adviceFile, closeFile, err := input(*advicePath, stdin)
	if err != nil {
		return err
	}
	defer closeFile()
	advice, err := ticketprep.DecodeAdvice(adviceFile)
	if err != nil {
		return err
	}
	packet := ticketprep.Packet{
		SchemaVersion:      ticketprep.SchemaVersion,
		KernelVersion:      ticketprep.KernelVersion,
		Capture:            capture,
		DeclaredProvenance: advice.DeclaredProvenance,
		Advice:             advice.Advice,
	}
	if err := packet.Validate(); err != nil {
		return err
	}
	return encodeOutput(*out, stdout, func(w io.Writer) error { return ticketprep.Encode(w, packet) })
}

func runFreshness(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("freshness", stderr)
	packetPath := fs.String("packet", "", "validated packet artifact")
	currentPath := fs.String("current", "", "supplied comparison capture artifact")
	out := fs.String("out", "-", "freshness output path, or - for stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *packetPath == "" || *currentPath == "" {
		return usageError("freshness requires -packet and -current")
	}
	packet, current, err := readPacketAndCapture(*packetPath, *currentPath, stdin)
	if err != nil {
		return err
	}
	report, err := ticketprep.Freshness(packet, current)
	if err != nil {
		return err
	}
	return encodeOutput(*out, stdout, func(w io.Writer) error { return ticketprep.Encode(w, report) })
}

func runRender(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("render", stderr)
	packetPath := fs.String("packet", "", "validated packet artifact")
	currentPath := fs.String("current", "", "supplied comparison capture artifact")
	dispositionPath := fs.String("dispositions", "", "complete action-item disposition artifact")
	out := fs.String("out", "-", "Markdown output path, or - for stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *packetPath == "" || *currentPath == "" {
		return usageError("render requires -packet and -current")
	}
	stdinUses := 0
	for _, path := range []string{*packetPath, *currentPath, *dispositionPath} {
		if path == "-" {
			stdinUses++
		}
	}
	if stdinUses > 1 {
		return usageError("only one render input may use stdin")
	}
	packet, current, err := readPacketAndCapture(*packetPath, *currentPath, stdin)
	if err != nil {
		return err
	}
	var dispositions *ticketprep.DispositionDocument
	if *dispositionPath != "" {
		file, closeFile, err := input(*dispositionPath, stdin)
		if err != nil {
			return err
		}
		defer closeFile()
		got, err := ticketprep.DecodeDispositions(file)
		if err != nil {
			return err
		}
		dispositions = &got
	}
	return encodeOutput(*out, stdout, func(w io.Writer) error {
		return ticketprep.Render(w, packet, current, dispositions)
	})
}

func readPacketAndCapture(packetPath, capturePath string, stdin io.Reader) (ticketprep.Packet, ticketprep.Capture, error) {
	if packetPath == "-" && capturePath == "-" {
		return ticketprep.Packet{}, ticketprep.Capture{}, usageError("only one input may use stdin")
	}
	packetFile, closePacket, err := input(packetPath, stdin)
	if err != nil {
		return ticketprep.Packet{}, ticketprep.Capture{}, err
	}
	defer closePacket()
	packet, err := ticketprep.DecodePacket(packetFile)
	if err != nil {
		return ticketprep.Packet{}, ticketprep.Capture{}, err
	}
	capture, err := readCapture(capturePath, stdin)
	return packet, capture, err
}

func readCapture(path string, stdin io.Reader) (ticketprep.Capture, error) {
	file, closeFile, err := input(path, stdin)
	if err != nil {
		return ticketprep.Capture{}, err
	}
	defer closeFile()
	return ticketprep.DecodeCapture(file)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ticketprep "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Sprintf("unexpected arguments %v", fs.Args()))
	}
	return nil
}

func usageError(message string) error { return fmt.Errorf("%w: %s", errUsage, message) }

func input(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open %s: %w", path, err)
	}
	return file, func() { _ = file.Close() }, nil
}

func encodeOutput(path string, stdout io.Writer, encode func(io.Writer) error) error {
	var body bytes.Buffer
	if err := encode(&body); err != nil {
		return err
	}
	if path == "-" {
		_, err := stdout.Write(body.Bytes())
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".ticketprep-*")
	if err != nil {
		return fmt.Errorf("create output beside %s: %w", path, err)
	}
	tempName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("set output mode: %w", err)
	}
	if _, err := temp.Write(body.Bytes()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync output: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace output %s: %w", path, err)
	}
	keep = true
	return nil
}
