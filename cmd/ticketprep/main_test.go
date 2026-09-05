package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/ticketprep"
)

func TestFourOperationsFormOneOfflineContract(t *testing.T) {
	repo := commandFixtureRepository(t)
	dir := t.TempDir()
	issuePath := filepath.Join(dir, "issue.json")
	capturePath := filepath.Join(dir, "capture.json")
	advicePath := filepath.Join(dir, "advice.json")
	packetPath := filepath.Join(dir, "packet.json")
	issue := ticketprep.IssueInput{
		SchemaVersion: ticketprep.SchemaVersion,
		Number:        7,
		URL:           "https://github.com/acme/widget/issues/7",
		Title:         "Change `UniqueThing`",
		Body:          "Keep `pkg/thing.go` covered.",
	}
	writeArtifact(t, issuePath, issue)

	assertRun(t, []string{"capture", "-repo", repo, "-issue", issuePath, "-out", capturePath}, "")
	captureFile, err := os.Open(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := ticketprep.DecodeCapture(captureFile)
	_ = captureFile.Close()
	if err != nil {
		t.Fatal(err)
	}

	advice := ticketprep.AdviceDocument{
		SchemaVersion: ticketprep.SchemaVersion,
		DeclaredProvenance: ticketprep.DeclaredProvenance{
			Provider: "openai", Model: "declared", Command: "$prep-ticket", Prompt: "one shot",
		},
		Advice: ticketprep.Advice{
			RestatedOutcome:         "Change the committed symbol.",
			CandidateNonGoals:       []string{},
			AssumptionsToConfirm:    []string{},
			DecisionQueue:           []ticketprep.Decision{},
			ApplicableConstraints:   []string{"Keep repository instructions."},
			AcceptanceGaps:          []string{},
			ProposedAcceptanceTests: []string{"Run make check."},
			AffectedAreaHypotheses:  []string{"pkg/thing.go"},
			CandidateDeliverySplits: []ticketprep.DeliverySplit{},
			Recommendation:          ticketprep.RecommendationInsufficient,
			Reasons:                 []string{"One contract question remains."},
		},
	}
	writeArtifact(t, advicePath, advice)
	assertRun(t, []string{"validate", "-capture", capturePath, "-advice", advicePath, "-out", packetPath}, "")

	freshness := assertRun(t, []string{"freshness", "-packet", packetPath, "-current", capturePath}, "")
	var report ticketprep.FreshnessReport
	if err := json.Unmarshal([]byte(freshness), &report); err != nil {
		t.Fatal(err)
	}
	if !report.SubjectMatches || !report.RepositoryMatches || len(report.Sections) == 0 {
		t.Fatalf("freshness = %+v", report)
	}
	rendered := assertRun(t, []string{"render", "-packet", packetPath, "-current", capturePath}, "")
	if !strings.Contains(rendered, "# Ticket preflight") || !strings.Contains(rendered, "ADVISORY ONLY") ||
		!strings.Contains(rendered, capture.Repository.Commit) || !strings.Contains(rendered, "TEST-01") {
		t.Fatalf("rendered packet:\n%s", rendered)
	}
}

func TestDocumentedAdviceExampleIsValid(t *testing.T) {
	document, err := os.ReadFile(filepath.Join(testModuleRoot(t), "docs", "TICKETPREP.md"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "The advisory input is:\n\n```json\n"
	_, example, ok := strings.Cut(string(document), marker)
	if !ok {
		t.Fatal("documented advisory input example not found")
	}
	example, _, ok = strings.Cut(example, "\n```")
	if !ok {
		t.Fatal("documented advisory input example has no closing fence")
	}
	if _, err := ticketprep.DecodeAdvice(strings.NewReader(example)); err != nil {
		t.Fatalf("documented advisory input is invalid: %v", err)
	}
}

func TestPilot152ArtifactsAndDispositionReproduce(t *testing.T) {
	root := testModuleRoot(t)
	pilot := filepath.Join(root, "docs", "ticketprep", "pilot-152")
	capturePath := filepath.Join(pilot, "capture.json")
	currentPath := filepath.Join(pilot, "current-capture.json")
	advicePath := filepath.Join(pilot, "advice.json")
	dispositionPath := filepath.Join(pilot, "dispositions.json")

	captureBody, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	currentBody, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(captureBody, currentBody) {
		t.Fatal("pilot original and supplied comparison captures differ")
	}

	dir := t.TempDir()
	generated := map[string]string{
		"packet.json":    filepath.Join(dir, "packet.json"),
		"freshness.json": filepath.Join(dir, "freshness.json"),
		"report.md":      filepath.Join(dir, "report.md"),
	}
	assertRun(t, []string{
		"validate", "-capture", capturePath, "-advice", advicePath, "-out", generated["packet.json"],
	}, "")
	assertRun(t, []string{
		"freshness", "-packet", generated["packet.json"], "-current", currentPath, "-out", generated["freshness.json"],
	}, "")
	assertRun(t, []string{
		"render", "-packet", generated["packet.json"], "-current", currentPath,
		"-dispositions", dispositionPath, "-out", generated["report.md"],
	}, "")
	for name, gotPath := range generated {
		want, err := os.ReadFile(filepath.Join(pilot, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(gotPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("regenerated %s differs from the retained pilot artifact", name)
		}
	}

	packetFile, err := os.Open(generated["packet.json"])
	if err != nil {
		t.Fatal(err)
	}
	packet, err := ticketprep.DecodePacket(packetFile)
	_ = packetFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	const approvedDigest = "sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be"
	digest, err := ticketprep.PacketDigest(packet)
	if err != nil {
		t.Fatal(err)
	}
	if digest != approvedDigest {
		t.Fatalf("pilot packet digest = %s, want %s", digest, approvedDigest)
	}

	dispositionFile, err := os.Open(dispositionPath)
	if err != nil {
		t.Fatal(err)
	}
	dispositions, err := ticketprep.DecodeDispositions(dispositionFile)
	_ = dispositionFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := dispositions.ValidateFor(packet); err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"DEC-01", "DEC-02", "DEC-03", "DEC-04", "DEC-05", "REC-01"}
	if len(dispositions.Items) != len(wantIDs) {
		t.Fatalf("pilot has %d dispositions, want %d", len(dispositions.Items), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		item := dispositions.Items[i]
		if item.SuggestionID != wantID || item.Disposition != ticketprep.DispositionAccepted {
			t.Errorf("disposition[%d] = %+v, want %s accepted", i, item, wantID)
		}
		wantOption := ""
		if i < 5 {
			wantOption = wantID + "-OPT-01"
		}
		if item.SelectedOptionID != wantOption {
			t.Errorf("disposition[%d] selected option = %q, want %q", i, item.SelectedOptionID, wantOption)
		}
	}
}

func TestFailedOperationDoesNotReplaceOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "packet.json")
	if err := os.WriteFile(out, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "-capture", "missing", "-advice", "missing", "-out", out}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep me" {
		t.Fatalf("failed operation replaced output with %q", body)
	}
}

func TestCommandUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want int
	}{
		{[]string{}, 2},
		{[]string{"unknown"}, 2},
		{[]string{"capture", "-h"}, 0},
		{[]string{"validate", "-capture", "-", "-advice", "-"}, 2},
	} {
		var stdout, stderr bytes.Buffer
		if got := run(context.Background(), tt.args, strings.NewReader(""), &stdout, &stderr); got != tt.want {
			t.Errorf("run(%v) = %d, want %d; stderr=%s", tt.args, got, tt.want, stderr.String())
		}
	}
}

func assertRun(t *testing.T, args []string, stdin string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr); code != 0 {
		t.Fatalf("ticketprep %v exited %d: %s", args, code, stderr.String())
	}
	return stdout.String()
}

func writeArtifact(t *testing.T, path string, value any) {
	t.Helper()
	var body bytes.Buffer
	if err := ticketprep.Encode(&body, value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

func commandFixtureRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	commandGit(t, repo, "init", "-q")
	commandGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"AGENTS.md":    "## Canonical commands\n\n```sh\nmake check\n```\n",
		"Makefile":     "check:\n\tgo test ./...\n",
		"pkg/thing.go": "package thing\nfunc UniqueThing() {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commandGit(t, repo, "add", ".")
	commandGit(t, repo, "commit", "-q", "-m", "fixture")
	return repo
}

func commandGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	argv := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-C", repo}, args...)
	cmd := exec.Command("git", argv...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ticketprep Test", "GIT_AUTHOR_EMAIL=ticketprep@example.invalid",
		"GIT_COMMITTER_NAME=Ticketprep Test", "GIT_COMMITTER_EMAIL=ticketprep@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	if body, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, body)
	}
}
