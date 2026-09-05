package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The #204 review leg of assembly, driven end to end: one daemon sweep against
// a forge, through the real reducer, the real forge client and the real
// reviewer execution boundary.
//
// What the fixtures replace is exactly the world outside the process — GitHub,
// and the model. Everything between is production code, which is the point:
// #11's rules are only preserved if the thing preserving them is the thing that
// ships.

const (
	rvOwner            = "acme"
	rvRepo             = "widgets"
	rvIssue            = 11
	rvPrincipal        = "ben-claim-bot"
	rvTracker          = "ben-tracker-bot"
	rvController       = "ben-review-bot"
	rvQueue            = "ben-queue"
	rvSecurity         = "security-approved"
	rvOccurrence       = int64(9001)
	rvApproval         = int64(900)
	rvSecurityApproval = int64(901)
	rvEpoch            = int64(7001)
	rvHead             = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	rvBase             = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	rvPRURL            = "https://github.com/acme/widgets/pull/42"
)

// reviewBlock is the workflow declaration under test, parameterized only by the
// reviewer command a fixture wants run.
func reviewBlock(argv string) string {
	return fmt.Sprintf(`credential_sources:
  reviewer:
    kind: static
    value: $BEN_TEST_REVIEW_TOKEN
review:
  enabled: true
  principal: %s
  tracker_author: %s
  controller: %s
  auth_source: reviewer
  reviewer_argv: %s
  reviewer_env: ["BEN_TEST_REVIEWER"]
  interval_ms: 3600000
`, rvPrincipal, rvTracker, rvController, argv)
}

// buildReviewLeg assembles the controller the way `ben run` does, pointed at a
// fixture forge.
func buildReviewLeg(t *testing.T, api string, argv string, requiredLabels ...string) *reviewLeg {
	return buildReviewLegFromBlock(t, reviewBlock(argv)+"  api_base_url: "+api+"\n", requiredLabels...)
}

func buildReviewLegFromBlock(t *testing.T, block string, requiredLabels ...string) *reviewLeg {
	t.Helper()
	t.Setenv("BEN_TEST_REVIEW_TOKEN", "review-controller-token")

	h := newHarness(t)
	h.b.reviewDir = filepath.Join(t.TempDir(), "reviews")
	h.b.source = func(name string) (core.SourceKind, bool) {
		if name != "static" {
			return nil, false
		}
		return credential.StaticKind{}, true
	}
	opts := []func(*workflowSpec){withReview(block)}
	if len(requiredLabels) > 0 {
		opts = append(opts, withRequiredLabels(requiredLabels...))
	}
	def := h.def(opts...)

	bundle, err := h.b.build(context.Background(), def, nil, everything)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_ = bundle
	leg := h.b.Review()
	if leg == nil {
		t.Fatal("an enabled review section produced no controller")
	}
	return leg
}

func profiledReviewBlock(api, argv string) string {
	return fmt.Sprintf(`credential_sources:
  reviewer:
    kind: static
    value: $BEN_TEST_REVIEW_TOKEN
review:
  enabled: true
  principal: %s
  tracker_author: %s
  controller: %s
  auth_source: reviewer
  reviewer_default_profile: deep
  reviewer_profiles:
    deep: %s
    fast: ["/bin/true"]
  reviewer_env: ["BEN_TEST_REVIEWER"]
  api_base_url: %s
  interval_ms: 3600000
`, rvPrincipal, rvTracker, rvController, argv, api)
}

func TestReviewUsesTheRemoteWorkspacesCompleteApprovalAnchor(t *testing.T) {
	g := newReviewForge(t)
	g.labels = append(g.labels, rvSecurity)
	g.approvals = append(g.approvals, reviewApproval{
		id: rvSecurityApproval, label: rvSecurity, createdAt: "2026-08-21T12:00:30Z",
	})
	srv := httptest.NewServer(g)
	defer srv.Close()

	t.Setenv("BEN_TEST_REVIEWER", strings.Join([]string{
		filepath.Join(t.TempDir(), "env"), filepath.Join(t.TempDir(), "count"), "changes_requested",
	}, "|"))
	leg := buildReviewLeg(t, srv.URL, reviewerArgvJSON(t), rvQueue, rvSecurity)
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	m, err := review.ParseReviewMarker(g.publishedBody)
	if err != nil {
		t.Fatal(err)
	}
	if m.Approval != rvSecurityApproval {
		t.Fatalf("review approval = %d, want complete-set workspace anchor %d", m.Approval, rvSecurityApproval)
	}
}

// One daemon poll, local backend: exactly one credential-stripped local Codex
// child, one advisory COMMENT review, one bounded route — and no GitHub Actions
// anywhere in it.
func TestOneDaemonSweepDrivesOneLocalReviewer(t *testing.T) {
	g := newReviewForge(t)
	srv := httptest.NewServer(g)
	defer srv.Close()

	// The child writes the verdict envelope on stdout, records the environment
	// it was actually given, and counts itself.
	envDump := filepath.Join(t.TempDir(), "env")
	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("BEN_TEST_REVIEWER", strings.Join([]string{envDump, countFile, "changes_requested"}, "|"))
	// A forge credential in the daemon's own environment, which must not reach
	// the reviewer under any name.
	t.Setenv("GITHUB_TOKEN", "a-tracker-credential-value")

	leg := buildReviewLeg(t, srv.URL, reviewerArgvJSON(t))
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got := readCount(t, countFile); got != 1 {
		t.Fatalf("the local reviewer ran %d times, want exactly one child per occurrence", got)
	}
	if g.published != 1 {
		t.Fatalf("published %d reviews, want exactly one", g.published)
	}
	m, err := review.ParseReviewMarker(g.publishedBody)
	if err != nil {
		t.Fatalf("the published review carries no marker: %v\n%s", err, g.publishedBody)
	}
	if m.Verdict != review.VerdictChangesRequested || m.Head != rvHead || m.Base != rvBase ||
		m.Occurrence != rvOccurrence || m.Claim != rvEpoch {
		t.Fatalf("review marker = %+v", m)
	}
	if len(g.assignees) != 0 {
		t.Errorf("assignees = %v; a changes-requested round hands the claim back", g.assignees)
	}
	if !contains(g.labels, rvQueue) {
		t.Error("the human's required label was removed on a revise route")
	}
	if len(g.posted) != 1 {
		t.Fatalf("posted %d comments, want the one route marker", len(g.posted))
	}
	route, err := review.ParseRouteMarker(g.posted[0])
	if err != nil || route.Outcome != review.OutcomeRevise {
		t.Fatalf("route marker = %+v (%v), want a revise", route, err)
	}

	// The reviewer held nothing. Read from the child's own environment dump
	// rather than from the allowlist that composed it.
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	env := string(dump)
	if strings.Contains(env, "a-tracker-credential-value") ||
		strings.Contains(env, "review-controller-token") {
		t.Fatalf("a credential reached the reviewer:\n%s", env)
	}
	for _, name := range append(reviewrun.ForbiddenEnv(), reviewrun.ProviderEnv()...) {
		if strings.Contains(env, name+"=") {
			t.Errorf("%s reached the reviewer", name)
		}
	}

	// A second sweep for the same occurrence must not review again: the route
	// marker is the idempotency key, for good.
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if got := readCount(t, countFile); got != 1 {
		t.Fatalf("a redelivered occurrence ran the reviewer %d times, want 1", got)
	}
	if g.published != 1 {
		t.Fatalf("a redelivered occurrence published %d reviews", g.published)
	}
}

func TestOneDaemonSweepUsesTheTicketSelectedReviewerProfile(t *testing.T) {
	g := newReviewForge(t)
	g.labels = append(g.labels, review.ReviewerProfileLabelPrefix+"deep")
	srv := httptest.NewServer(g)
	defer srv.Close()

	envDump := filepath.Join(t.TempDir(), "env")
	t.Setenv("BEN_TEST_REVIEWER", strings.Join([]string{envDump, "", "clean"}, "|"))
	leg := buildReviewLegFromBlock(t, profiledReviewBlock(srv.URL, reviewerArgvJSON(t)))
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, err := review.ParseReviewMarker(g.publishedBody)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReviewerProfile != "deep" {
		t.Fatalf("published profile = %q, want deep", m.ReviewerProfile)
	}
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dump), "BEN_REVIEW_PROFILE=deep") {
		t.Fatalf("reviewer environment carries no profile evidence:\n%s", dump)
	}
}

// A reviewer that states nothing routes nothing. The occurrence stays unrouted
// and the next sweep looks again, which is the availability model.
func TestASilentReviewerRoutesNothing(t *testing.T) {
	g := newReviewForge(t)
	srv := httptest.NewServer(g)
	defer srv.Close()

	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("BEN_TEST_REVIEWER", strings.Join([]string{"", countFile, "silent"}, "|"))

	leg := buildReviewLeg(t, srv.URL, reviewerArgvJSON(t))
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if g.published != 0 || len(g.posted) != 0 {
		t.Fatalf("a silent reviewer published %d reviews and %d comments", g.published, len(g.posted))
	}
	if len(g.assignees) != 1 || !contains(g.labels, rvQueue) {
		t.Fatalf("a silent reviewer mutated the issue: assignees=%v labels=%v", g.assignees, g.labels)
	}
	if got := readCount(t, countFile); got != 1 {
		t.Fatalf("the reviewer ran %d times", got)
	}
}

func TestReviewControllerCredentialRotatesWithoutDaemonRestart(t *testing.T) {
	g := newReviewForge(t)
	srv := httptest.NewServer(g)
	defer srv.Close()

	t.Setenv("BEN_TEST_REVIEWER", "||silent")
	leg := buildReviewLeg(t, srv.URL, reviewerArgvJSON(t))
	const rotated = "review-controller-token-rotated"
	t.Setenv("BEN_TEST_REVIEW_TOKEN", rotated)
	if err := leg.controller.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	g.mu.Lock()
	auth := append([]string(nil), g.auth...)
	g.mu.Unlock()
	if len(auth) == 0 {
		t.Fatal("the sweep made no forge request")
	}
	for i, got := range auth {
		if got != "Bearer "+rotated {
			t.Fatalf("request %d Authorization = %q, want the rotated credential", i, got)
		}
	}
}

// Everything the controller needs is proven at assembly, before a published
// milestone depends on it. A guidance file that is not there is the case worth
// naming: it changes what every round is judged against, so discovering it at
// the first review would mean one round judged by a different standard.
func TestAnEnabledControllerRefusesAtAssembly(t *testing.T) {
	t.Setenv("BEN_TEST_REVIEW_TOKEN", "review-controller-token")
	h := newHarness(t)
	h.b.reviewDir = filepath.Join(t.TempDir(), "reviews")
	h.b.source = func(name string) (core.SourceKind, bool) {
		if name != "static" {
			return nil, false
		}
		return credential.StaticKind{}, true
	}
	block := reviewBlock(reviewerArgvLiteral()) +
		"  api_base_url: https://api.github.test\n  guidance_file: /nonexistent/guidance.md\n"
	def := h.def(withReview(block))
	if _, err := h.b.build(context.Background(), def, nil, everything); !errors.Is(err, ErrNotReady) {
		t.Fatalf("build = %v, want ErrNotReady", err)
	}
}

// An omitted section is a daemon with no controller and no review state at all.
func TestAWorkflowWithoutAReviewSectionBuildsNoController(t *testing.T) {
	h := newHarness(t)
	h.b.reviewDir = filepath.Join(t.TempDir(), "reviews")
	if _, err := h.b.build(context.Background(), h.def(), nil, everything); err != nil {
		t.Fatalf("build: %v", err)
	}
	if leg := h.b.Review(); leg != nil {
		t.Fatalf("a workflow with no review section built a controller: %+v", leg)
	}
	if _, err := os.Stat(h.b.reviewDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a daemon with no controller created %s", h.b.reviewDir)
	}
}

// A sandbox has no host environment to pass through, and BEN never exports one
// into a machine it does not own. Refused at assembly rather than ignored.
func TestARemoteReviewerRefusesAHostPassthrough(t *testing.T) {
	t.Setenv("BEN_TEST_REVIEW_TOKEN", "review-controller-token")
	h := newHarness(t)
	h.b.reviewDir = filepath.Join(t.TempDir(), "reviews")

	def := h.def(withReview(reviewBlock(reviewerArgvLiteral())))
	// The declaration says the reviewer may be handed a host variable, which is
	// legitimate locally and is not expressible against a backend.
	if len(def.Config.Review.ReviewerEnv) == 0 {
		t.Fatal("the fixture declares no passthrough")
	}
	substrate, err := airlock.New(airlock.Options{
		BaseURL: "https://airlock.test", Profile: "ben-agent",
		Auth:  credential.NewLiteral("test", "token-value"),
		Store: airlock.NewDirStore(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := &orchestrator.Bundle{Definition: def}
	if _, err := h.b.buildReviewSession(def, substrate, bundle, nil, "acme/widgets"); err == nil ||
		!strings.Contains(err.Error(), "reviewer_env") {
		t.Fatalf("buildReviewSession = %v, want a refusal naming review.reviewer_env", err)
	}
}

func TestRemoteCodexReviewRequiresReadOnlySandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		ok   bool
	}{
		{name: "read only", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "-"}, ok: true},
		{name: "read only equals", argv: []string{"/usr/local/bin/codex", "exec", "--json", "--sandbox=read-only", "-"}, ok: true},
		{name: "pinned model and effort", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "--model", "gpt-5.6-sol", "-c", `model_reasoning_effort="xhigh"`, "-"}, ok: true},
		{name: "missing sandbox", argv: []string{"codex", "exec", "--json", "-"}},
		{name: "missing json", argv: []string{"codex", "exec", "--sandbox", "read-only", "-"}},
		{name: "missing stdin marker", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only"}},
		{name: "workspace write", argv: []string{"codex", "exec", "--json", "--sandbox", "workspace-write", "-"}},
		{name: "dangerous", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--dangerously-bypass-approvals-and-sandbox", "-"}},
		{name: "yolo alias", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--yolo", "-"}},
		{name: "automatic workspace write", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--approve-for-me", "-"}},
		{name: "automatic workspace write alias", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--not-so-yolo", "-"}},
		{name: "additional writable root", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--add-dir", "/tmp", "-"}},
		{name: "additional writable root equals", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--add-dir=/tmp", "-"}},
		{name: "full auto", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--full-auto", "-"}},
		{name: "sandbox config override", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "-c", `sandbox_mode="danger-full-access"`, "-"}},
		{name: "config newline injection", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "-c", "model_reasoning_effort=\"xhigh\"\nsandbox_mode=\"danger-full-access\"", "-"}},
		{name: "output write", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--output-last-message", "/tmp/result", "-"}},
		{name: "profile override", argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "--profile", "unsafe", "-"}},
		{name: "wrong subcommand", argv: []string{"codex", "review", "--json", "--sandbox", "read-only", "-"}},
		{name: "other model neutral reviewer", argv: []string{"reviewer", "--mode", "managed"}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireReadOnlyRemoteCodex(tc.argv)
			if (err == nil) != tc.ok {
				t.Fatalf("requireReadOnlyRemoteCodex(%v) = %v, ok=%v", tc.argv, err, tc.ok)
			}
		})
	}
}

func TestEveryNamedRemoteProfileMustBeReadOnly(t *testing.T) {
	rc := config.ReviewConfig{ReviewerProfiles: map[string][]string{
		"deep": {"codex", "exec", "--json", "--sandbox", "read-only", "-"},
		"fast": {"codex", "exec", "--json", "--sandbox", "workspace-write", "-"},
	}}
	checked := 0
	var refused string
	for field, argv := range reviewerInvocations(rc) {
		checked++
		if err := requireReadOnlyRemoteCodex(argv); err != nil {
			refused = field
		}
	}
	if checked != 2 || refused != "review.reviewer_profiles.fast" {
		t.Fatalf("checked %d profiles and refused %q, want both checked and fast refused", checked, refused)
	}
}

func TestRemoteReviewUsesTheWorkspaceCanonicalRepositoryIdentity(t *testing.T) {
	h := startRemoteE2E(t, true)
	got, err := reviewRepositoryIdentity(&airlock.Substrate{}, &orchestrator.Bundle{Workspaces: h.Provider}, rvOwner+"/"+rvRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got != h.Mirror.Repository() {
		t.Fatalf("remote review repository = %q, want mirror identity %q", got, h.Mirror.Repository())
	}
	if got == rvOwner+"/"+rvRepo {
		t.Fatal("the regression fixture does not distinguish forge owner/repo from the backend claim identity")
	}
}

func reviewerArgvLiteral() string { return `["/bin/true"]` }

// reviewerArgvJSON names this test binary's own helper as the reviewer, which
// is how the composed environment and the child count are observed.
func reviewerArgvJSON(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal([]string{os.Args[0], "-test.run=TestReviewHelperProcess", "-test.v=false"})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestReviewHelperProcess is the reviewer child. `BEN_TEST_REVIEWER` carries
// `<env dump path>|<count path>|<verdict or "silent">`.
func TestReviewHelperProcess(t *testing.T) {
	spec := os.Getenv("BEN_TEST_REVIEWER")
	if spec == "" {
		t.Skip("not the helper process")
	}
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("malformed helper spec %q", spec)
	}
	if parts[0] != "" {
		if err := os.WriteFile(parts[0], []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if parts[1] != "" {
		body, _ := os.ReadFile(parts[1])
		if err := os.WriteFile(parts[1], append(body, 'x'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if parts[2] == "silent" {
		return
	}
	fmt.Printf("looked at the diff\n%s\n{\"verdict\":%q,\"findings\":\"one finding\"}\n%s\n",
		reviewrun.VerdictOpen, parts[2], reviewrun.VerdictClose)
}

func readCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(body)
}

func contains(set []string, want string) bool {
	for _, s := range set {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// reviewForge serves the endpoints the controller uses and applies the writes,
// so a second sweep reads back what the first one did — including the
// change-log events a real write produces, which is what recovery resumes from.
type reviewForge struct {
	t *testing.T

	mu            sync.Mutex
	assignees     []string
	labels        []string
	posted        []string
	published     int
	publishedBody string
	unassignedAt  time.Time
	reviews       []map[string]any
	auth          []string
	approvals     []reviewApproval
}

type reviewApproval struct {
	id        int64
	label     string
	createdAt string
}

func newReviewForge(t *testing.T) *reviewForge {
	return &reviewForge{
		t: t, assignees: []string{rvPrincipal}, labels: []string{rvQueue},
		approvals: []reviewApproval{{id: rvApproval, label: rvQueue, createdAt: "2026-08-21T12:00:00Z"}},
	}
}

func (g *reviewForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")

	base := "/repos/" + rvOwner + "/" + rvRepo
	switch r.Method + " " + r.URL.Path {
	case "GET " + base + "/issues":
		g.write(w, []map[string]any{{"number": rvIssue, "state": "open"}})

	case "GET " + base + "/pulls":
		g.write(w, []map[string]any{})

	case "GET " + base + "/issues/11":
		g.write(w, map[string]any{
			"number": rvIssue, "state": "open",
			"labels":    labelObjects(g.labels),
			"assignees": userObjects(g.assignees),
		})

	case "GET " + base + "/issues/11/comments":
		comments := []map[string]any{{
			"id": 1, "user": map[string]any{"login": rvTracker},
			"body": "**BEN published a pull request.**\n\n- pull request: " + rvPRURL +
				fmt.Sprintf("\n- daemon: `d`\n<!-- ben:milestone kind=published occurrence=%d -->\n", rvOccurrence),
			"created_at": "2026-08-21T12:05:00Z",
		}}
		for i, posted := range g.posted {
			comments = append(comments, map[string]any{
				"id": 100 + i, "user": map[string]any{"login": rvController},
				"body": posted, "created_at": "2026-08-21T12:30:00Z",
			})
		}
		g.write(w, comments)

	case "GET " + base + "/issues/11/events":
		var events []map[string]any
		for _, approval := range g.approvals {
			events = append(events, map[string]any{
				"id": approval.id, "event": "labeled", "actor": map[string]any{"login": "a-human"},
				"label": map[string]any{"name": approval.label}, "created_at": approval.createdAt,
			})
		}
		events = append(events,
			map[string]any{"id": rvEpoch, "event": "assigned", "actor": map[string]any{"login": rvTracker},
				"assignee": map[string]any{"login": rvPrincipal}, "created_at": "2026-08-21T12:01:00Z"},
			map[string]any{"id": rvOccurrence, "event": "unlabeled", "actor": map[string]any{"login": rvTracker},
				"label": map[string]any{"name": "ben:claimed"}, "created_at": "2026-08-21T12:04:00Z"},
		)
		if !g.unassignedAt.IsZero() {
			events = append(events, map[string]any{
				"id": 9500, "event": "unassigned", "actor": map[string]any{"login": rvController},
				"assignee":   map[string]any{"login": rvPrincipal},
				"created_at": g.unassignedAt.UTC().Format(time.RFC3339),
			})
		}
		g.write(w, events)

	case "GET " + base + "/pulls/42":
		g.write(w, map[string]any{
			"number": 42, "html_url": rvPRURL, "state": "open", "merged": false,
			"body": "Fixes #11",
			"head": map[string]any{"ref": "ben/11", "sha": rvHead},
			"base": map[string]any{"ref": "main", "sha": rvBase},
		})

	case "GET " + base + "/pulls/42/reviews":
		g.write(w, g.reviews)

	case "GET " + base + "/compare/" + rvBase + "..." + rvHead:
		io.WriteString(w, "diff --git a/x b/x\n+changed\n") //nolint:errcheck // test fixture

	case "POST " + base + "/pulls/42/reviews":
		var payload struct{ Body, CommitID string }
		_ = json.Unmarshal(body, &struct {
			Body     *string `json:"body"`
			CommitID *string `json:"commit_id"`
		}{&payload.Body, &payload.CommitID})
		g.published++
		g.publishedBody = payload.Body
		g.reviews = append(g.reviews, map[string]any{
			"id": 7, "user": map[string]any{"login": rvController}, "body": payload.Body,
			"commit_id": payload.CommitID, "state": review.ReviewStateCommented,
			"submitted_at": "2026-08-21T12:10:00Z",
		})
		g.write(w, map[string]any{"id": 8})

	case "DELETE " + base + "/issues/11/assignees":
		var payload struct {
			Assignees []string `json:"assignees"`
		}
		_ = json.Unmarshal(body, &payload)
		for _, login := range payload.Assignees {
			var kept []string
			for _, a := range g.assignees {
				if !strings.EqualFold(a, login) {
					kept = append(kept, a)
				}
			}
			g.assignees = kept
		}
		g.unassignedAt = time.Date(2026, 8, 21, 12, 20, 0, 0, time.UTC)
		g.write(w, map[string]any{})

	case "POST " + base + "/issues/11/comments":
		var payload struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &payload)
		g.posted = append(g.posted, payload.Body)
		g.write(w, map[string]any{"id": 5})

	default:
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/issues/11/labels/") {
			name := strings.TrimPrefix(r.URL.Path, base+"/issues/11/labels/")
			var kept []string
			for _, l := range g.labels {
				if !strings.EqualFold(l, name) {
					kept = append(kept, l)
				}
			}
			g.labels = kept
			g.write(w, []map[string]any{})
			return
		}
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *reviewForge) write(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		g.t.Error(err)
	}
}

func labelObjects(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func userObjects(logins []string) []map[string]any {
	out := make([]map[string]any, 0, len(logins))
	for _, l := range logins {
		out = append(out, map[string]any{"login": l})
	}
	return out
}
