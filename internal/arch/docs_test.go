package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The documentation invariants a reviewer would otherwise have to notice.
//
// Two of them are about where operator-facing content lives (#162): the
// unattended-operation runbook is docs/DEPLOY.md's, and the README carries a
// pointer that still refuses the one action a reader must not take. The third
// is about links, and it is the anchor the other two lean on — it is driven by
// the filesystem rather than by a list here, so it fails when a document moves
// no matter which document moved.
//
// The rest are about AGENTS.md, which #161 split the same way: the rules stay,
// the evidence behind them moved to docs/. That split has an inverse failure —
// a rule that becomes a link is a rule an agent no longer reads by default —
// so the rules are asserted present, the evidence absent, and the file's whole
// length capped.
//
// Per AGENTS.md, a test driven by the declaration it checks proves only that
// the declared entries hold. TestMarkdownLinksResolve,
// TestRepoMapNamesEveryDocsFile, TestMutatingProtectionCommandsCarryTheirWarning,
// TestEveryDocsFileIsLinkedFromTheRoot, TestAGENTSStaysWithinItsContextBudget
// and TestAGENTSSectionsNamedElsewhereExist are each anchored at a boundary
// that no table below controls: every markdown file, every file in docs/, every
// mutating branch-protection command, every link into docs/, AGENTS.md's line
// count, and every quoted cross-reference to one of its sections anywhere in
// the repository.

// docLine is one line of a markdown file and whether it sits inside a fenced
// code block. Both readers below need the distinction: docs/DEPLOY.md's shell
// block contains `# 2. the branch rule`, which is a comment to a reader and an
// H1 to anything matching on a leading hash.
type docLine struct {
	text   string
	fenced bool
	number int // 1-indexed, for messages
}

func readDoc(t *testing.T, path string) []docLine {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []docLine
	fenced := false
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			fenced = !fenced
			out = append(out, docLine{text: line, fenced: true, number: i + 1})
			continue
		}
		out = append(out, docLine{text: line, fenced: fenced, number: i + 1})
	}
	return out
}

// paragraphs joins each run of non-fenced, non-blank lines into one unit,
// numbered by its first line.
//
// Markdown hard-wraps at column ~100 here, so a line break inside a paragraph
// is not a content boundary — and the first version of this file matched
// per-line, which silently checked nothing for every link whose text wrapped.
// `[README's Status\nsection](../README.md#status)` is a link to a reader, two
// unmatchable fragments to a per-line regexp, and a passing test either way.
func paragraphs(t *testing.T, path string) []docLine {
	t.Helper()
	var out []docLine
	var buf []string
	start := 0
	flush := func() {
		if len(buf) > 0 {
			out = append(out, docLine{text: strings.Join(buf, " "), number: start})
			buf = nil
		}
	}
	for _, line := range readDoc(t, path) {
		if line.fenced || strings.TrimSpace(line.text) == "" {
			flush()
			continue
		}
		if len(buf) == 0 {
			start = line.number
		}
		buf = append(buf, strings.TrimSpace(line.text))
	}
	flush()
	return out
}

// flatten collapses every whitespace run to a single space, so a phrase this
// file looks for is found whether or not a rewrap put a newline through it.
func flatten(s string) string {
	return strings.TrimSpace(whitespace.ReplaceAllString(s, " "))
}

var whitespace = regexp.MustCompile(`\s+`)

// markdownFiles returns every markdown file in this module, module-relative and
// slash-separated, under the same scoping rules the import walk uses: dotted
// and underscored directories, testdata, vendor, and nested modules are out.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			if ignoredPackageDir(d.Name()) || isModuleRoot(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no markdown files found; the walk is checking nothing")
	}
	return out
}

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// headingSlug renders a heading the way GitHub anchors it: lower-cased,
// punctuation dropped, spaces hyphenated. `-` and `_` survive; `§`, backticks
// and commas do not.
func headingSlug(heading string) string {
	heading = markdownLink.ReplaceAllString(heading, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

func headingSlugs(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range readDoc(t, path) {
		if line.fenced {
			continue
		}
		text := strings.TrimSpace(line.text)
		if !strings.HasPrefix(text, "#") {
			continue
		}
		out[headingSlug(strings.TrimLeft(text, "# "))] = true
	}
	return out
}

// Every relative link in every markdown file resolves — to a file that exists,
// and where the link names an anchor, to a heading that file actually carries.
//
// This is the check behind #162's "internal links to the moved anchors are
// updated". Written as a property of the repository rather than as a grep for
// the anchors that moved this time: a link to a section is stale the moment the
// section moves, and the next move will be somebody else's.
func TestMarkdownLinksResolve(t *testing.T) {
	root := moduleRoot(t)
	checked, anchored := 0, 0
	for _, file := range markdownFiles(t, root) {
		for _, para := range paragraphs(t, filepath.Join(root, file)) {
			for _, m := range markdownLink.FindAllStringSubmatch(para.text, -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				checked++
				path, anchor, _ := strings.Cut(target, "#")

				resolved := filepath.Join(root, filepath.Dir(file), filepath.FromSlash(path))
				if path == "" { // a same-file anchor
					resolved = filepath.Join(root, filepath.FromSlash(file))
				}
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s:%d links to %q, which does not exist", file, para.number, target)
					continue
				}
				if anchor == "" || !strings.HasSuffix(resolved, ".md") {
					continue
				}
				anchored++
				if !headingSlugs(t, resolved)[anchor] {
					t.Errorf("%s:%d links to %q, but %s carries no heading anchored #%s",
						file, para.number, target, path, anchor)
				}
			}
		}
	}
	// Both halves of the walk report "pass" by finding nothing, so each says
	// how much it looked at. The anchor half is the one #162 needs and the one
	// a stray change to the extractor would quietly switch off.
	if checked == 0 {
		t.Error("no relative markdown link was checked; the extractor matched nothing")
	}
	if anchored == 0 {
		t.Error("no link into a heading was checked; the anchor half of this test is inert")
	}
}

// The unattended-operation runbook is a document of its own, and the README is
// a README (#162).
//
// The procedure markers are two-sided on purpose: each must be in
// docs/DEPLOY.md *and* absent from README.md. One side alone would pass while
// the content sat in both places, which is the state the move exists to end —
// and a duplicated runbook is worse than either home, because the two drift and
// neither says which is current.
func TestTheUnattendedRunbookLivesInDocs(t *testing.T) {
	root := moduleRoot(t)
	deploy := flatten(read(t, filepath.Join(root, "docs", "DEPLOY.md")))
	readme := flatten(read(t, filepath.Join(root, "README.md")))

	for _, tc := range []struct {
		marker string
		why    string
	}{
		{"useradd", "provisioning the dedicated §10.1 account"},
		{"require_code_owner_reviews", "the approval no agent credential can produce"},
		{"dismiss_stale_reviews", "binding the approval to the commit that merges"},
		{"require_last_push_approval", "binding the approval to the commit that merges"},
		{"enforce_admins", "the honest part of the trade"},
		{"restrictions", "why the push allowlist is deliberately null"},
		{"sandbox_mode", "the isolation that is configurable today"},
		{"WorkspaceProvider", "the container strategy that is not"},
		{"accepted_because", "what risk-accepted mode requires a workflow to record"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			if !strings.Contains(deploy, tc.marker) {
				t.Errorf("docs/DEPLOY.md does not mention %q — %s went missing in the move", tc.marker, tc.why)
			}
			if strings.Contains(readme, tc.marker) {
				t.Errorf("README.md mentions %q; the runbook lives in docs/DEPLOY.md, and content in both places drifts", tc.marker)
			}
		})
	}
}

// The README's pointer names the two rules rather than only linking out.
//
// A reader who never opens the link must still not hand-apply a protection
// rule, and must still know that requiring *a* review is not the requirement.
// Both are things whose absence costs a control and shows up nowhere.
func TestTheREADMEPointerNamesTheRulesItPointsAt(t *testing.T) {
	readme := flatten(read(t, filepath.Join(moduleRoot(t), "README.md")))

	for _, tc := range []struct {
		want string
		why  string
	}{
		{"docs/DEPLOY.md", "the pointer itself"},
		{"code owners", "requiring a review is not requiring one the agent cannot supply"},
		{"CODEOWNERS", "the file that makes the code-owner rule cover every path"},
		{"Terraform is the only writer", "for BEN's own repository, the API is not a writer at all"},
		{"`PATCH`", "the mutating verbs the reader must not send"},
		{"reverted", "what happens to a hand-applied rule, silently and at exit 0"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if !strings.Contains(readme, tc.want) {
				t.Errorf("README.md's deployment pointer does not say %q — %s", tc.want, tc.why)
			}
		})
	}

	// The pointer is a pointer. A README that grew the procedure back would
	// pass every assertion above.
	if mutatingGHAPI.MatchString(readme) {
		t.Error("README.md carries a mutating `gh api` command; the runbook and its warnings live in docs/DEPLOY.md")
	}
}

// A mutating branch-protection command against the API, anywhere in the
// repository's prose.
var mutatingGHAPI = regexp.MustCompile(`gh api\s+(-X|--method)\s+(PUT|PATCH|DELETE)`)

// warningReach is how far above a mutating command its warning may sit and
// still be met on the way to it — roughly one screen. The warning's whole value
// is being read *before* somebody hand-applies a rule (#162), so distance is
// the property, not mere presence somewhere in the file.
const warningReach = 60

const terraformWarning = "Terraform is the only writer"

// Every mutating branch-protection command in the repo is preceded, within
// reach, by the Terraform-only-writer warning.
//
// Scanning every markdown file rather than the one that has the command today:
// the failure this guards against is somebody pasting a working `PUT` into a
// new document, which is exactly the case a test naming docs/DEPLOY.md would
// not see.
func TestMutatingProtectionCommandsCarryTheirWarning(t *testing.T) {
	root := moduleRoot(t)
	found := 0
	for _, file := range markdownFiles(t, root) {
		lines := readDoc(t, filepath.Join(root, file))
		for i, line := range lines {
			if !mutatingGHAPI.MatchString(line.text) {
				continue
			}
			found++
			from := max(0, i-warningReach)
			var window []string
			for _, above := range lines[from:i] {
				window = append(window, above.text)
			}
			// Joined rather than matched line by line, so a rewrap that puts a
			// newline through the warning does not read as its absence.
			if !strings.Contains(flatten(strings.Join(window, " ")), terraformWarning) {
				t.Errorf("%s:%d is a mutating branch-protection command with no %q warning in the %d lines above it; "+
					"a hand-applied rule is reverted silently by the next Atlantis apply",
					file, line.number, terraformWarning, warningReach)
			}
		}
	}
	// The regexp is the whole test; a typo in it would pass everywhere.
	if found == 0 {
		t.Errorf("no mutating branch-protection command found in any markdown file, so %q matched nothing", mutatingGHAPI)
	}
}

// AGENTS.md's repo map names every file in docs/.
//
// Driven by the directory, not by the row: a new document that the map does not
// mention is the failure, and a test reading the row's entries back could not
// see it (#157, #162).
func TestRepoMapNamesEveryDocsFile(t *testing.T) {
	root := moduleRoot(t)
	var row string
	for _, line := range readDoc(t, filepath.Join(root, "AGENTS.md")) {
		if strings.HasPrefix(strings.TrimSpace(line.text), "| `docs` |") {
			row = line.text
			break
		}
	}
	if row == "" {
		t.Fatal("AGENTS.md's repo map has no `docs` row")
	}

	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.Contains(row, e.Name()) {
			t.Errorf("AGENTS.md's repo map does not name docs/%s", e.Name())
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// agentsSections splits AGENTS.md into its `##` sections, keyed by title, each
// flattened to one string carrying its own `###` subsections and code blocks.
//
// Sections rather than the whole file, because "the rule is still stated" is
// not the property that matters on its own: a rule restated three sections away
// from the link that replaced its evidence is a rule nobody meets in context.
// Fenced lines are body, never headings — the audit command in "Go on a dev
// machine" is inside a code block, and a `#` comment in a shell block is a
// comment.
func agentsSections(t *testing.T, root string) map[string]string {
	t.Helper()
	lines := map[string][]string{}
	var order []string
	title := ""
	for _, line := range readDoc(t, filepath.Join(root, "AGENTS.md")) {
		text := strings.TrimSpace(line.text)
		if !line.fenced && strings.HasPrefix(text, "## ") {
			title = strings.TrimSpace(strings.TrimPrefix(text, "##"))
			order = append(order, title)
			continue
		}
		if title != "" {
			lines[title] = append(lines[title], line.text)
		}
	}
	if len(order) == 0 {
		t.Fatal("AGENTS.md has no `##` sections; the split found nothing")
	}
	out := map[string]string{}
	for title, body := range lines {
		out[title] = flatten(strings.Join(body, " "))
	}
	return out
}

// Every rule AGENTS.md carried before #161 is still stated in AGENTS.md, in the
// section that carries it — not demoted to a link into docs/.
//
// This is the inverse of the extraction's goal and the thing it can quietly
// break: CLAUDE.md points at AGENTS.md, so what is written here is read on
// every turn, and what moved to docs/ is read only by someone who follows the
// link. Evidence can afford that. A prohibition cannot.
//
// The table is the enumeration #161 asked the PR for, kept where it can fail.
// It proves the listed rules survive, and nothing about a rule nobody listed —
// hence the budget and cross-reference anchors below.
func TestEveryRuleStaysInAGENTS(t *testing.T) {
	sections := agentsSections(t, moduleRoot(t))

	for _, tc := range []struct {
		section string
		rule    string
		why     string
	}{
		{"Go on a dev machine", "Do not persist a Go setting you also export", "the rule itself"},
		{"Go on a dev machine", "Pick the file or the shell, never both", "which of the two to give up"},
		{"Go on a dev machine", "go env GOENV", "what is persisted, if anything"},
		{"Go on a dev machine", "env -i", "the audit that answers for a daemon rather than for your shell"},
		{"Go on a dev machine", "GOPRIVATE", "what may be persisted"},
		{"Go on a dev machine", "GO111MODULE", "what may not — and the one that bit us"},
		{"Go on a dev machine", "GOFLAGS", "the sharpest of them: it changes what every build compiles"},

		{"Toolchain", "minor-level", "the shape of the floor"},
		{"Toolchain", "naming a patch needs an argument", "the rule for narrowing it"},
		{"Toolchain", "preference needs the same argument", "and the rule for adding a `toolchain` line"},
		{"Toolchain", "internal/arch", "what fails until either is recorded deliberately"},

		{"Working in worktrees", "git worktree add", "how ticket work starts"},
		{"Working in worktrees", "A branch is checked out in exactly one worktree", "the rule itself"},
		{"Working in worktrees", "never reuse one worktree across branches", "the second half of it"},
		{"Working in worktrees", "--ignore-other-worktrees", "a flag that overrides git's refusal"},
		{"Working in worktrees", "`-B`", "the flag that bites, named"},
		{"Working in worktrees", "`-C`", "the same hazard under `git switch`"},
		{"Working in worktrees", "git branch -f", "and under `git branch`"},
		{"Working in worktrees", "git switch --detach", "what to reach for instead"},
		{"Working in worktrees", "make worktree-check", "what detects the duplicate"},
		{"Working in worktrees", "Diagnose before touching anything", "every repair destroys the evidence"},
		{"Working in worktrees", "if none matches, stop", "a tree that matches no recent origin/main commit contains local work"},
		{"Working in worktrees", "git stash create", "what never to run while investigating"},
		{"Working in worktrees", "Clear the duplicate first", "or the repair is refused and strands this worktree"},
		{"Working in worktrees", "git stash -a", "the only stash that parks ignored files"},
		{"Working in worktrees", "`-a`, not `-u`", "the distinction that loses work when missed"},
		{"Working in worktrees", "git ls-files --others", "the enumeration that reports nothing at risk while data goes"},
		{"Working in worktrees", "reset --hard HEAD", "the repair that moves no ref"},
		{"Working in worktrees", "never to `origin/main`", "the one that does"},
		{"Working in worktrees", "git pull --ff-only", "and when that is the right repair"},
		{"Working in worktrees", "outside the repository", "where a worktree goes"},
		{"Working in worktrees", "hygiene, not correctness", "which of the two rules this one is"},
		{"Working in worktrees", "EnterWorktree", "the Claude Code tool whose location is not configurable"},

		// The links are rules' company, not their replacement; each must be in
		// the section whose evidence it holds.
		{"Go on a dev machine", "docs/GO-ENV.md", "the evidence for this section"},
		{"Toolchain", "docs/TOOLCHAIN.md", "the evidence for this section"},
		{"Working in worktrees", "docs/WORKTREES.md", "the evidence for this section"},
	} {
		t.Run(tc.section+"/"+tc.rule, func(t *testing.T) {
			body, ok := sections[tc.section]
			if !ok {
				t.Fatalf("AGENTS.md has no %q section", tc.section)
			}
			if !strings.Contains(body, tc.rule) {
				t.Errorf("AGENTS.md's %q section does not state %q — %s. #161 moved evidence to docs/, "+
					"not rules: a rule behind a link is one an agent no longer reads by default",
					tc.section, tc.rule, tc.why)
			}
		})
	}
}

// The evidence #161 extracted is in the document that now owns it, and gone
// from AGENTS.md.
//
// Two-sided for the reason #162's runbook markers are: one side alone passes
// while the material sits in both places, which is the state the extraction
// exists to end — and duplicated prose drifts with nothing saying which copy is
// current. These are measurements, incidents and rejected alternatives, so
// unlike the rules above they are exactly what a link is for.
func TestExtractedEvidenceLeftAGENTS(t *testing.T) {
	root := moduleRoot(t)
	agents := flatten(read(t, filepath.Join(root, "AGENTS.md")))

	for _, tc := range []struct {
		doc     string
		marker  string
		why     string
		section string
	}{
		{"WORKTREES.md", "ben-b11", "the worktree that crossed main", "Working in worktrees"},
		{"WORKTREES.md", "b568c4f", "the commit the primary tree skipped", "Working in worktrees"},
		{"WORKTREES.md", "shutdown.go", "a file that never arrived", "Working in worktrees"},
		{"WORKTREES.md", "15:06:01", "the mtime table that diagnoses it", "Working in worktrees"},
		{"WORKTREES.md", "Fast-forward", "what git reported on the way", "Working in worktrees"},
		{"WORKTREES.md", "rev-list", "the loop that proves what the tree is", "Working in worktrees"},
		{"WORKTREES.md", "No local changes to save", "what `stash -u` says about an ignored file", "Working in worktrees"},
		{"WORKTREES.md", "isModuleRoot", "what the arch walk's scoping is actually for", "Working in worktrees"},

		{"GO-ENV.md", "os.UserConfigDir", "where the persisted file lives", "Go on a dev machine"},
		{"GO-ENV.md", "GO111MODULE=off", "the setting that killed the first dogfood run", "Go on a dev machine"},
		{"GO-ENV.md", "2023", "when it was written, and how long it hid", "Go on a dev machine"},
		{"GO-ENV.md", "after_create", "why BEN's hook detects this instead of overriding it", "Go on a dev machine"},

		{"TOOLCHAIN.md", "setup-go", "what CI resolves the version with", "Toolchain"},
		{"TOOLCHAIN.md", "1.26.2", "the patch-level row of the resolution table", "Toolchain"},
		{"TOOLCHAIN.md", "go1.26.9", "the ignored-then-downloaded row", "Toolchain"},
		{"TOOLCHAIN.md", "never downgrades", "why a preference does not pin anything", "Toolchain"},
	} {
		t.Run(tc.doc+"/"+tc.marker, func(t *testing.T) {
			doc := flatten(read(t, filepath.Join(root, "docs", tc.doc)))
			if !strings.Contains(doc, tc.marker) {
				t.Errorf("docs/%s does not mention %q — %s went missing in the move", tc.doc, tc.marker, tc.why)
			}
			if strings.Contains(agents, tc.marker) {
				t.Errorf("AGENTS.md still carries %q; it belongs to docs/%s, and content in both places drifts "+
					"while every agent pays for this copy on every turn", tc.marker, tc.doc)
			}
		})
	}
}

// Every file in docs/ is linked from a document at the repository root.
//
// Driven by the directory rather than by a list of the three files #161 added:
// the failure is an extracted document nothing points at, and a reference
// nobody reaches is worse than the paragraph it replaced. Where the link sits
// is asserted per-section above; that it exists at all is asserted here, where
// a new document cannot avoid it. TestMarkdownLinksResolve holds the other end,
// that each link goes somewhere real.
func TestEveryDocsFileIsLinkedFromTheRoot(t *testing.T) {
	root := moduleRoot(t)
	linked := map[string]bool{}
	for _, from := range []string{"AGENTS.md", "README.md"} {
		for _, para := range paragraphs(t, filepath.Join(root, from)) {
			for _, m := range markdownLink.FindAllStringSubmatch(para.text, -1) {
				path, _, _ := strings.Cut(m[1], "#")
				if name, ok := strings.CutPrefix(path, "docs/"); ok {
					linked[name] = true
				}
			}
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !linked[e.Name()] {
			t.Errorf("no document at the repository root links to docs/%s; an extracted reference nobody "+
				"points at is worse than the paragraph it replaced", e.Name())
		}
	}
}

// CLAUDE.md points at AGENTS.md, so the whole file is loaded into every agent's
// context on every turn — the single largest fixed context cost in the repo,
// paid by tasks that never go near most of it. That is what #161 cut, from 352
// lines to 244 by moving three sections' evidence to docs/.
//
// The ceiling is a budget, not a style rule. When it fails, the question is
// which kind of material arrived: a rule belongs in AGENTS.md (and something
// else should leave), while the measurement, incident or rejected alternative
// behind one belongs in docs/, linked from the section that states the rule.
// Raising the number is the third legitimate answer — deliberately, in the
// change that needs it, which is all this test asks.
const agentsLineBudget = 260

func TestAGENTSStaysWithinItsContextBudget(t *testing.T) {
	lines := strings.Count(read(t, filepath.Join(moduleRoot(t), "AGENTS.md")), "\n")
	if lines > agentsLineBudget {
		t.Errorf("AGENTS.md is %d lines, over its %d-line budget. It is loaded into every agent's context "+
			"on every turn: move the evidence behind a rule to docs/ and link it from the section that "+
			"states the rule, or raise agentsLineBudget deliberately here (#161)", lines, agentsLineBudget)
	}
}

// A quoted AGENTS.md section name, anywhere in the repository's prose or
// comments — go.mod's `see AGENTS.md, <quoted section>` and WORKFLOW.md's
// parenthesized form, including the Makefile's backslash-escaped quotes.
//
// The separator before the quote is required, not optional: without it the
// closing quote of a Go string literal holding the path AGENTS.md reads as the
// opening quote of a section name, and this file is full of those.
var agentsSectionRef = regexp.MustCompile(`AGENTS\.md(?:'s)?[,:]?\s+\(?\s*\\?"([^"\\]{2,60})\\?"`)

// referencedFile reports whether a file is worth scanning for cross-references
// to AGENTS.md: the ones that carry prose about how this repo is worked.
func referencedFile(name string) bool {
	if name == "Makefile" {
		return true
	}
	switch filepath.Ext(name) {
	case ".md", ".go", ".mod", ".yml", ".sh":
		return true
	}
	return false
}

// commentText strips a leading comment marker so a reference wrapped across two
// comment lines reads as one sentence. WORKFLOW.md carries exactly that: its
// pointer to the dev-machine section breaks mid-name, with the second half
// behind the next line's comment marker.
func commentText(line string) string {
	text := strings.TrimSpace(line)
	for _, marker := range []string{"//", "#"} {
		if stripped, ok := strings.CutPrefix(text, marker); ok {
			return strings.TrimSpace(stripped)
		}
	}
	return text
}

// Every section of AGENTS.md that another file names in quotes still exists.
//
// The Makefile, go.mod and WORKFLOW.md all send a reader to a section by name
// rather than by link, so a rename or a removal breaks them silently — and #161
// restructured every section they name. Anchored on the repository: the failure
// is a dangling reference, whichever file grows one next.
func TestAGENTSSectionsNamedElsewhereExist(t *testing.T) {
	root := moduleRoot(t)
	titles := headingTitles(t, filepath.Join(root, "AGENTS.md"))

	found := 0
	for _, file := range referencingFiles(t, root) {
		var lines []string
		for _, line := range strings.Split(read(t, filepath.Join(root, file)), "\n") {
			lines = append(lines, commentText(line))
		}
		text := flatten(strings.Join(lines, " "))
		for _, m := range agentsSectionRef.FindAllStringSubmatch(text, -1) {
			found++
			if !titles[m[1]] {
				t.Errorf("%s sends a reader to AGENTS.md's %q section, which AGENTS.md has no heading for",
					file, m[1])
			}
		}
	}
	// The regexp is the whole test; a typo in it would pass everywhere.
	if found == 0 {
		t.Errorf("no quoted AGENTS.md section reference found anywhere, so %q matched nothing", agentsSectionRef)
	}
}

// referencingFiles returns every file worth scanning for the reference above,
// module-relative, under the same scoping rules the import and markdown walks
// use.
func referencingFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (ignoredPackageDir(d.Name()) || isModuleRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !referencedFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// headingTitles returns every heading in a markdown file by its text, at any
// level — the form another file names a section by, where headingSlugs returns
// the form a link anchors to.
func headingTitles(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range readDoc(t, path) {
		if line.fenced {
			continue
		}
		text := strings.TrimSpace(line.text)
		if !strings.HasPrefix(text, "#") {
			continue
		}
		out[strings.TrimSpace(strings.TrimLeft(text, "#"))] = true
	}
	return out
}
