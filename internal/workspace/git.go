package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// outputLimit bounds how much captured git/hook output an error carries; the
// tail is kept because git and shells put the cause last.
const outputLimit = 4096

// gitCredentialHelper feeds the tracker credential (SPEC §10.2) to git via
// the credential protocol. The secret travels in the child environment —
// never in process arguments (`ps`) and never in on-disk git config.
const gitCredentialHelper = `!f() { if [ "$1" = get ]; then printf 'username=%s\npassword=%s\n' "$BEN_REMOTE_USERNAME" "$BEN_REMOTE_PASSWORD"; fi; }; f`

// gitNoAutoMaintenance turns off git's background maintenance for every git BEN
// starts (#154).
//
// `fetch`, `commit` and `merge` all end by running auto-maintenance, and git
// *detaches* what that starts, so it outlives the command BEN waited for — a
// pack can land in objects/pack after the fetch returned. What BEN then has is a
// process it did not start and cannot account for, running as the daemon's user,
// taking gc.pid and pack locks in base.git and in the linked worktrees sharing
// its object store, while an attempt may be running in one. Not corruption: a
// process outside BEN's account of the workspaces it owns, which is what SPEC
// §9.10 is careful about everywhere else.
//
// Both keys, because they stop different things — maintenance.auto=false stops
// the fork itself, and gc.auto=0 stops the work, which is the leg that answers
// for a git old enough to have run `git gc --auto` directly, before `git
// maintenance` existed to be routed through. As `-c`
// rather than base.git's config, because `-c` outranks every config file: the
// guarantee then holds over a base repository BEN did not create, cannot be
// edited out of one, and reaches the git processes git itself starts, through
// the GIT_CONFIG_PARAMETERS it exports for them. Maintaining the base repository,
// if it is ever wanted, is then a decision BEN makes rather than git's default.
// docs/WORKTREES.md carries what each of those was measured from.
//
// Bounded to the git BEN starts, which is what BEN can account for: a hook's
// shell (SPEC §6.5) and an agent's run (§7.6) are given composed environments of
// their own, and a git either of them starts is theirs.
var gitNoAutoMaintenance = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}

// gitArgv composes the full argv of a BEN-invoked git: the no-maintenance
// overrides, which must precede the subcommand, then what the caller asked for.
// Every exec of git in this package goes through it; error messages keep naming
// the caller's own args, because the overrides are not part of the request.
func gitArgv(args []string) []string {
	argv := make([]string, 0, len(gitNoAutoMaintenance)+len(args))
	argv = append(argv, gitNoAutoMaintenance...)
	return append(argv, args...)
}

// gitCmd runs one git command and returns its trimmed combined output.
// Errors carry the redacted argv and output — git's stderr is the only
// diagnostic there is.
//
// `secret` is the credential *this invocation used*, and empty for the local
// reads that use none. Passed per call rather than held on the provider, which
// is the whole of the fix: a value captured at construction scrubs a **stale**
// token after a rotation while the live one flows through git's stderr into
// error text and logs (SPEC §6.2, amendment 6).
func (p *Provider) gitCmd(ctx context.Context, dir, secret string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitArgv(args)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), extraEnv...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(redact(string(out), secret))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s",
			redact(strings.Join(args, " "), secret), err, truncateOutput(text))
	}
	return text, nil
}

func (p *Provider) git(ctx context.Context, dir string, args ...string) (string, error) {
	return p.gitCmd(ctx, dir, "", nil, args...)
}

// remoteGit runs a git command that contacts the remote, obtaining the
// credential immediately before the invocation and injecting it through the
// helper + environment (SPEC §10.2 — secrets stay out of argv). The inherited
// helper list is cleared first so the credential source is deterministic.
//
// The credential is resolved here, per invocation, and the same value is what
// redaction covers. A source failure returns **before git runs at all**: there
// is no unauthenticated fallback to fall through to, which matters most against
// a public remote, where an unauthenticated fetch would quietly succeed and hide
// the misconfiguration until the first private operation.
func (p *Provider) remoteGit(ctx context.Context, dir string, args ...string) (string, error) {
	if p.authSource == nil {
		return p.gitCmd(ctx, dir, "", nil, args...)
	}
	auth, err := p.authSource.Auth(ctx)
	if err != nil {
		return "", fmt.Errorf("git %s: obtaining the remote credential: %w", strings.Join(args, " "), err)
	}
	full := append([]string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + gitCredentialHelper,
	}, args...)
	env := []string{
		"BEN_REMOTE_USERNAME=" + auth.Username,
		"BEN_REMOTE_PASSWORD=" + auth.Password,
	}
	return p.gitCmd(ctx, dir, auth.Password, env, full...)
}

// baseGit serializes every git invocation against base.git: worktree
// metadata operations (add/prune/remove) race each other across issues, and
// git's own locking turns that into spurious failures rather than safety.
func (p *Provider) baseGit(ctx context.Context, args ...string) (string, error) {
	p.locks.baseMu.Lock()
	defer p.locks.baseMu.Unlock()
	return p.git(ctx, p.baseDir, args...)
}

// gitLines runs one git command against base.git and returns at most maxLines of
// its stdout, each truncated to maxLineBytes, reporting whether there was more.
//
// Bounded at the *source*, not after the fact, and that is the whole point of it
// existing beside gitCmd. CombinedOutput buffers everything git chooses to write
// before anyone can cap it, and two of the things git writes here are
// agent-authored and unbounded: a commit subject is one line of a message the
// agent composed, and `git diff --name-only` has no `-n`. A repository an agent
// controls could therefore make a read whose result is discarded a moment later
// cost gigabytes of daemon memory (#61 review, finding 1).
//
// It stops reading as soon as it has enough, and the deferred cancel then takes
// the child down — so an enormous listing costs the bound, not the listing. stderr
// is captured separately and capped, because it is the only diagnostic there is.
//
// The lock is bounded too: baseMu is held across fetches, so a caller that must be
// able to give up cannot Lock (see lockUntil).
func (p *Provider) gitLines(ctx context.Context, maxLines, maxLineBytes int, args ...string) (lines []string, more bool, err error) {
	unlock, err := lockUntil(ctx, &p.locks.baseMu)
	if err != nil {
		return nil, false, fmt.Errorf("git %s: waiting for the base repository lock: %w",
			strings.Join(args, " "), err)
	}
	defer unlock()

	// Cancelled on every return, which is what makes "stop reading" also mean "stop
	// writing": without it a child still producing output would block on a full
	// pipe with nobody draining it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "git", gitArgv(args)...)
	cmd.Dir = p.baseDir
	cmd.Env = gitEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	stderr := tailWriter{limit: outputLimit}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	lines, truncated, stoppedShort, readErr := boundedLines(stdout, maxLines, maxLineBytes)
	if stoppedShort {
		// We have enough and are not going to read the rest. Cancelling now takes
		// the child down, rather than leaving it blocked writing into a pipe nobody
		// drains — and it is done *only* on this path, because it is what makes the
		// exit status below unreadable.
		cancel()
	}
	waitErr := cmd.Wait()

	if readErr != nil {
		return nil, false, fmt.Errorf("git %s: reading output: %w",
			strings.Join(args, " "), readErr)
	}
	if waitErr != nil {
		if stoppedShort {
			// We killed it, so its status is ours and says nothing about the lines
			// already read. Not conflated with the ordinary failure below, and this
			// is the distinction the early version got wrong: cancelling
			// unconditionally made every exit status look like one we had caused,
			// which would have turned `merge-base --is-ancestor`'s exit 1 — a
			// *verdict* — into a silent empty success.
			return lines, truncated, nil
		}
		return nil, false, fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), waitErr, truncateOutput(stderr.String()))
	}
	return lines, truncated, nil
}

// boundedLines reads newline-separated lines from r, keeping at most maxLines of
// them and at most maxLineBytes of each. It reports whether anything was left out,
// and separately whether it stopped before the reader was exhausted.
//
// Fixed buffers throughout: a bufio.Scanner would refuse an over-long line rather
// than truncate it (`token too long`), and bufio.Reader.ReadString would buffer the
// whole of it — either way the caller pays for a line whose length the agent chose.
//
// It reads one line *past* the bound, so "exactly at the bound" and "more than the
// bound" are distinguishable: a listing of exactly maxLines entries is complete, and
// reporting it as cut would be the same silent-inaccuracy defect as not reporting a
// real cut.
//
// Each kept line is cut on a rune boundary. It ends up inside a fence, and a split
// rune in a fence is a fence that may not survive rendering (SPEC §5.6).
func boundedLines(r io.Reader, maxLines, maxLineBytes int) (out []string, truncated, stoppedShort bool, err error) {
	var (
		line []byte
		buf  = make([]byte, 4096)
	)
	for {
		n, readErr := r.Read(buf)
		for _, b := range buf[:n] {
			if b != '\n' {
				if len(line) < maxLineBytes {
					line = append(line, b)
				} else {
					// The rest of this line is dropped rather than buffered. The
					// read continues: a pathological line degrades the account, and
					// the caller's own context is what bounds the time.
					truncated = true
				}
				continue
			}
			out = append(out, trimPartialRune(string(line)))
			line = line[:0]
			if len(out) > maxLines {
				return out[:maxLines], true, true, nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, false, false, readErr
		}
	}
	// A trailing line with no terminator. git newline-terminates every record these
	// callers ask for, so this is defence against a format that does not.
	if len(line) > 0 {
		out = append(out, trimPartialRune(string(line)))
		if len(out) > maxLines {
			return out[:maxLines], true, false, nil
		}
	}
	return out, truncated, false, nil
}

// trimPartialRune drops an incomplete UTF-8 sequence from the end of s, which is
// what a byte-counted cut can leave behind.
//
// The test is whether the *last rune decodes*, not whether the last byte looks
// like a lead byte. An earlier version walked back over every trailing
// continuation byte first, which turned "café" into "caf" and "x日本" into "x日":
// it destroyed complete characters, silently, on lines that were never cut at all
// (#61 re-review, finding 2). A cut is reported; this is not a place to make one.
func trimPartialRune(s string) string {
	if complete(s) {
		return s
	}
	// Incomplete: either a lead byte whose continuations were cut off, or
	// continuation bytes whose lead byte was. Either way at most UTFMax-1 bytes
	// stand between here and the last rune that does decode.
	for range utf8.UTFMax - 1 {
		if len(s) == 0 {
			return s
		}
		s = s[:len(s)-1]
		if complete(s) {
			return s
		}
	}
	return s
}

// complete reports whether s ends on a whole rune. An empty string does.
func complete(s string) bool {
	if s == "" {
		return true
	}
	r, size := utf8.DecodeLastRuneInString(s)
	// RuneError with size 1 is the decoder's "these bytes are not a rune"; a real
	// U+FFFD in the input decodes with size 3 and is left alone.
	return r != utf8.RuneError || size > 1
}

// gitEnv is the daemon environment minus repo-context variables that would
// redirect git away from the paths passed explicitly, plus a guard against
// interactive credential prompts hanging the daemon.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch key, _, _ := strings.Cut(kv, "="); key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE":
			continue
		default:
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// revParse resolves ref to the commit SHA it peels to. Absence (ok=false,
// nil error) is distinguished from every other failure: with --quiet, git's
// "no such ref" verdict is a silent exit 1 — but an unreadable ref store
// produces the same silence, so absence is only accepted after the store
// proves readable. Broken refs, refs over missing objects, permission
// failures, and cancellation are errors, never absence, so damage cannot
// slip into absence/recreate paths (SPEC §6.6 fail-closed; #16).
func (p *Provider) revParse(ctx context.Context, ref string) (string, bool, error) {
	out, err := p.baseGit(ctx, "rev-parse", "--verify", "--quiet", ref)
	if err == nil && out != "" {
		// Peel to the commit: our contract is commit SHAs, and a ref left
		// pointing at an annotated tag must not leak the tag object ID. A
		// peel failure is damage, not absence.
		peeled, peelErr := p.baseGit(ctx, "rev-parse", "--verify", "--quiet", out+"^{commit}")
		if peelErr != nil || peeled == "" {
			return "", false, fmt.Errorf("%w: ref %s points at unusable object %s: %v",
				ErrBaseRepoState, ref, out, peelErr)
		}
		return peeled, true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, ctxErr
	}
	var exitErr *exec.ExitError
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && out == "" {
		if storeErr := p.refStoreReadable(ref); storeErr != nil {
			return "", false, fmt.Errorf("%w: cannot prove ref %s absent: %v",
				ErrBaseRepoState, ref, storeErr)
		}
		return "", false, nil
	}
	return "", false, fmt.Errorf("%w: cannot resolve ref %s: %v", ErrBaseRepoState, ref, err)
}

// refStoreReadable reports whether the files-backend storage that could
// hold ref is readable: the loose ref file, its ancestor directories, and
// packed-refs must each be openable or genuinely absent. git reports an
// unreadable store with the same silent exit 1 as a missing ref, so this
// check is what separates "absent" from "cannot know" (#16). Lstat comes
// first because Open follows symlinks: a dangling link ENOENTs like a
// missing path, but its target could reappear — that is "cannot know".
func (p *Provider) refStoreReadable(ref string) error {
	openable := func(path string) error {
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// The entry exists (possibly as a symlink); it must fully resolve
		// and open — any failure here, dangling links included, is trouble.
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		f.Close()
		return nil
	}
	loose := filepath.Join(p.baseDir, filepath.FromSlash(ref))
	if err := openable(loose); err != nil {
		return err
	}
	for dir := filepath.Dir(loose); ; dir = filepath.Dir(dir) {
		if err := openable(dir); err != nil {
			return err
		}
		if dir == p.baseDir || dir == filepath.Dir(dir) {
			break
		}
	}
	return openable(filepath.Join(p.baseDir, "packed-refs"))
}

// isAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant. Exit status 1 is git's "no" verdict; anything else — missing
// objects, corruption, cancellation — is repository trouble, never a verdict
// (SPEC §6.6 fail-closed).
func (p *Provider) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := p.baseGit(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	return p.ancestorAnswer(ctx, ancestor, descendant, err)
}

// ancestorAnswer classifies what `merge-base --is-ancestor` said.
//
// **Exit status 1 is git's "no". Every other failure is trouble, never a verdict**
// — a missing object, a corrupt repository, a revision that does not resolve — and
// SPEC §6.6 fails closed on all of them. Shared by both callers because the two
// drifted: the bounded reader accepted *any* ExitError as "not an ancestor", which
// turned a corrupt repository into a confident empty account and put "committed
// nothing" in front of an agent about a branch nobody could read (#61 re-review,
// finding 3).
func (p *Provider) ancestorAnswer(ctx context.Context, ancestor, descendant string, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Our own deadline or the daemon shutting down, not an answer about git.
		return false, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%w: cannot order %s against %s: %v", ErrBaseRepoState, ancestor, descendant, err)
}

// worktreeEntry is one block of `git worktree list --porcelain` output.
type worktreeEntry struct {
	path   string
	branch string // full ref (refs/heads/…); empty when detached or bare
	bare   bool
}

// listWorktrees parses `git worktree list --porcelain`, the normative source
// of registration truth (SPEC §6.6).
func (p *Provider) listWorktrees(ctx context.Context) ([]worktreeEntry, error) {
	out, err := p.baseGit(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []worktreeEntry
	var cur *worktreeEntry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeEntry{path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			// Attribute lines only make sense inside a block; ignore strays.
		case line == "bare":
			cur.bare = true
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(line, "branch ")
		}
	}
	flush()
	return entries, nil
}

// redact strips one credential (SPEC §10.2) from text before it can reach an
// error message or log line — defense in depth behind the environment-only
// delivery.
//
// A function of the value rather than a method on the provider, because the
// provider no longer holds one: the credential a remote invocation used is
// known only to that invocation, and a provider-lifetime copy is exactly the
// stale value this exists to stop leaking.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

func truncateOutput(s string) string {
	if len(s) <= outputLimit {
		return s
	}
	return "…" + s[len(s)-outputLimit:]
}
