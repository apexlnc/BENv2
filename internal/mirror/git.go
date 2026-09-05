package mirror

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/gitcmd"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

// outputLimit bounds how much captured git output an error carries; the tail is
// kept because git puts the cause last.
const outputLimit = 4096

// gitIn runs one git command and returns its trimmed combined output, with the
// store's serialization held for the duration.
//
// `secret` is the credential *this invocation used*, empty for the local reads
// that use none. Per call rather than held on the mirror, which is the whole of
// the point: a value captured at construction scrubs a stale token after a
// rotation while the live one flows through git's stderr into error text and
// logs.
func (m *Mirror) gitIn(ctx context.Context, dir, secret string, extraEnv []string, args ...string) (string, error) {
	m.locks.git.Lock()
	defer m.locks.git.Unlock()
	return m.gitUnlocked(ctx, dir, secret, append(gitcmd.Env(), extraEnv...), args...)
}

// env is complete rather than an overlay so callers must choose the local or
// remote authority set before reaching the shared executor.
func (m *Mirror) gitUnlocked(ctx context.Context, dir, secret string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitcmd.Argv(args)...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(m.redact(string(out), secret))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s",
			m.redact(strings.Join(args, " "), secret), err, truncateOutput(text))
	}
	return text, nil
}

// git runs a local read against the store.
func (m *Mirror) git(ctx context.Context, args ...string) (string, error) {
	return m.gitIn(ctx, m.gitDir, "", nil, args...)
}

// remoteGit runs a git command that contacts the canonical remote, obtaining the
// credential immediately before the invocation and injecting it through the
// helper plus environment (SPEC §10.2 — secrets stay out of argv). The inherited
// helper list is cleared first so the credential source is deterministic.
//
// A source failure returns **before git runs at all**, and it returns the
// source's own error unflattened, so core.CredentialFailure can still classify
// it: §9.7's one exception to fail-closed is a *transient* credential failure
// retried in `verifying`, and a wrapper that lost the class would turn that into
// a parked claim. There is no unauthenticated fallback — against a public remote
// an unauthenticated fetch would quietly succeed and hide the misconfiguration
// until the first private operation.
func (m *Mirror) remoteGit(ctx context.Context, args ...string) (string, error) {
	m.locks.git.Lock()
	defer m.locks.git.Unlock()
	return m.remoteGitUnlocked(ctx, args...)
}

// remoteGitUnlocked is remoteGit with the store lock already held. Keeping
// credential resolution inside that lock is load-bearing: a short-lived token
// must not age while another fetch owns the invocation slot.
//
// The helper and the scope it answers for come from internal/gitremote, shared
// with internal/workspace rather than copied: it is one shell script, and its
// host check (#230) is the kind of thing two copies drift apart on — this store
// has no `origin` and names the URL per invocation, so nothing on disk would
// even hint that one of the two had stopped scoping.
func (m *Mirror) remoteGitUnlocked(ctx context.Context, args ...string) (string, error) {
	if m.authSource == nil {
		return m.gitUnlocked(ctx, m.gitDir, "", gitcmd.RemoteEnv(), args...)
	}
	auth, err := m.authSource.Auth(ctx)
	if err != nil {
		return "", fmt.Errorf("mirror: obtaining the fetch credential for %s: %w", m.repository, err)
	}
	full := append(gitremote.CredentialConfig(), args...)
	env := append(gitcmd.RemoteEnv(), gitremote.CredentialEnv(m.remoteURL, auth.Username, auth.Password)...)
	return m.gitUnlocked(ctx, m.gitDir, auth.Password, env, full...)
}

// lsRemote asks the canonical remote for one exact ref path and returns the
// object it names, reporting absence separately.
//
// Exact, and counted. `ls-remote` takes patterns, and a pattern can match more
// than one ref; this asks for a full path and accepts an answer only when
// exactly one line names exactly that path. Two answers for one path is
// ErrRefAmbiguous rather than a first-match, because nothing here is entitled to
// pick which of them the run published (SPEC §9.7 fail closed).
//
// An empty answer is the protocol's absence verdict; a failed probe is an error
// and never absence.
func (m *Mirror) lsRemote(ctx context.Context, ref string) (string, bool, error) {
	out, err := m.remoteGit(ctx, "ls-remote", "--", m.remoteURL, ref)
	if err != nil {
		return "", false, fmt.Errorf("mirror: probing %s for %s: %w", m.repository, ref, err)
	}
	found := refMatches(out, ref)
	switch len(found) {
	case 0:
		return "", false, nil
	case 1:
		return found[0], true, nil
	default:
		return "", false, fmt.Errorf("%w: %s answered with %d objects for %s",
			ErrRefAmbiguous, m.repository, len(found), ref)
	}
}

// refMatches picks the objects an ls-remote answer names for one exact ref
// path.
//
// Exact rather than prefixed, which is what excludes the peeled `^{}` entry a
// remote advertises beside an annotated tag — a second line for one name that a
// prefix match would read as ambiguity. Split out from lsRemote because it is
// the half that can be tested against an answer no reachable remote will
// produce on demand.
func refMatches(out, ref string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && name == ref && sha != "" {
			found = append(found, sha)
		}
	}
	return found
}

// defaultBranch reads the canonical remote's HEAD symref — the branch a fresh
// claim's base is taken from when the issue branch does not exist yet, mirroring
// what the v1 strategy pins at its first prepare (SPEC §6.2, §9.7).
func (m *Mirror) defaultBranch(ctx context.Context) (string, error) {
	out, err := m.remoteGit(ctx, "ls-remote", "--symref", m.remoteURL, "HEAD")
	if err != nil {
		return "", fmt.Errorf("mirror: resolving the default branch of %s: %w", m.repository, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: refs/heads/"); ok {
			if name, _, found := strings.Cut(rest, "\t"); found && name != "" {
				return name, nil
			}
		}
	}
	// The identity is credential-free by construction (identify), so naming the
	// repository here cannot leak the URL the daemon fetched with.
	return "", fmt.Errorf("%w: %s reported no default branch", ErrBaseBranchNotFound, m.repository)
}

// fetchInto fetches one remote ref into one local non-branch ref and returns the
// object the local ref then names.
//
// The returned SHA is read back from the *local* ref rather than reported from
// the fetch, so every SHA this package hands out names an object the store
// actually holds. That is what makes the ancestry question answerable at all: a
// commit reported by a remote and absent from the store is a string, and
// `merge-base` against a string is an error rather than a verdict.
func (m *Mirror) fetchInto(ctx context.Context, remoteRef, localRef string) (string, error) {
	// A successful RecordClaim authorizes a remote run, so the fetched objects
	// and the ref that pins them must survive the same system crash as the claim
	// record written afterwards. Git's platform default can omit loose objects,
	// and its macOS default method is writeout-only; override both for this
	// mutation rather than walking and re-syncing the whole object store.
	if _, err := m.remoteGit(ctx,
		"-c", "core.fsync=all",
		"-c", "core.fsyncMethod=fsync",
		// Keep the object-directory durability boundary finite. Any objects this
		// fetch adds land in objects/pack rather than arbitrary loose-object fanout
		// directories, so hardenFetch can publish every new directory entry without
		// walking the whole object store.
		"-c", "fetch.unpackLimit=1",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-write-commit-graph",
		m.remoteURL, "+"+remoteRef+":"+localRef); err != nil {
		return "", fmt.Errorf("mirror: fetching %s from %s: %w", remoteRef, m.repository, err)
	}
	if err := m.hardenFetch(localRef); err != nil {
		return "", err
	}
	sha, ok, err := m.revParse(ctx, localRef)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: fetched %s but %s did not materialize", ErrMirrorState, remoteRef, localRef)
	}
	return sha, nil
}

// hardenFetch publishes the directory entries behind Git's fsynced data.
// core.fsync flushes the pack, its index and the ref lock file, but the files
// ref backend then renames the lock into place; a system crash can lose a rename
// whose parent directory was never synced. Objects first and ref last preserves
// the same ordering as the claim record: no durable name points at data whose
// directory entry has not landed yet.
func (m *Mirror) hardenFetch(localRef string) error {
	objects := filepath.Join(m.gitDir, "objects")
	for _, dir := range []string{filepath.Join(objects, "pack"), objects} {
		if err := m.syncDirectory(dir); err != nil {
			return fmt.Errorf("mirror: making fetched objects durable: %w", err)
		}
	}

	refPath := filepath.Join(m.gitDir, filepath.FromSlash(localRef))
	for dir := filepath.Dir(refPath); ; dir = filepath.Dir(dir) {
		if err := m.syncDirectory(dir); err != nil {
			return fmt.Errorf("mirror: making fetched ref %s durable: %w", localRef, err)
		}
		if dir == m.gitDir {
			break
		}
		if parent := filepath.Dir(dir); parent == dir {
			return fmt.Errorf("%w: ref %s resolves outside the store", ErrMirrorState, localRef)
		}
	}
	return nil
}

// revParse resolves ref to the commit it peels to, distinguishing absence from
// every other failure.
//
// The distinction is the one internal/workspace draws and for the same reason: a
// missing ref and an unreadable ref store are both a silent exit 1, and reading
// the second as the first turns damage into "there is no publication". Here the
// store is BEN's own and holds no worktrees, so the ref backend cannot be racing
// a checkout — but the classification still has to hold, because absence is a
// fact this package reports and a caller acts on.
func (m *Mirror) revParse(ctx context.Context, ref string) (string, bool, error) {
	out, err := m.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil && out != "" {
		return out, true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, ctxErr
	}
	var exitErr *exec.ExitError
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && out == "" {
		return "", false, nil
	}
	return "", false, fmt.Errorf("%w: cannot resolve ref %s: %v", ErrMirrorState, ref, err)
}

// deleteRef removes a ref if it is there, and says nothing if it was not.
func (m *Mirror) deleteRef(ctx context.Context, ref string) error {
	if _, ok, err := m.revParse(ctx, ref); err != nil || !ok {
		return err
	}
	if _, err := m.git(ctx, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("%w: cannot remove ref %s: %v", ErrMirrorState, ref, err)
	}
	return nil
}

// isAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant, over objects this store holds.
//
// Exit status 1 is git's "no". Every other failure is trouble and never a
// verdict — a missing object, a corrupt store, a revision that does not resolve
// — which is the classification internal/workspace arrived at the hard way
// (#61): a looser one turns a store nobody could read into a confident negative,
// and a confident negative here is a contradiction routed to a human about a
// publication that may be perfectly good.
func (m *Mirror) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := m.git(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%w: cannot order %s against %s: %v", ErrMirrorState, ancestor, descendant, err)
}

// redact strips the configured fetch URL and one credential from text before
// either can reach an error message or a log line. Git may print a URL without
// its conventional .git suffix, so both spellings are covered. The allowed
// repository identity is added by callers separately.
func (m *Mirror) redact(s, secret string) string {
	for _, remote := range []string{m.remoteURL, strings.TrimSpace(m.remoteURL)} {
		if remote == "" {
			continue
		}
		s = strings.ReplaceAll(s, remote, "<remote>")
		withoutSlash := strings.TrimSuffix(remote, "/")
		withoutDotGit := strings.TrimSuffix(withoutSlash, ".git")
		if withoutDotGit != "" && withoutDotGit != remote {
			s = strings.ReplaceAll(s, withoutDotGit, "<remote>")
		}
	}
	return redactCredential(s, secret)
}

// redactCredential is defense in depth behind environment-only delivery.
func redactCredential(s, secret string) string {
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
