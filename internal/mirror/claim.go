package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// claimRecord is one claim's pin as it lives on disk.
//
// Everything in it is bounded and non-secret: identifiers the tracker already
// publishes, a branch name derived from one of them, a commit SHA, and the
// credential-free repository identity. The remote URL is deliberately absent —
// it is reconstructible by nobody from here, which is the point — and so is
// anything about the run that produced the publication, because none of that
// would be evidence.
type claimRecord struct {
	Issue        string    `json:"issue"`
	Key          string    `json:"key"`
	Epoch        int64     `json:"epoch"`
	Branch       string    `json:"branch"`
	BaseSHA      string    `json:"base_sha"`
	TargetBranch string    `json:"target_branch,omitempty"`
	Repository   string    `json:"repository"`
	RecordedAt   time.Time `json:"recorded_at"`
}

func (r claimRecord) claim() core.RemoteClaim {
	return core.RemoteClaim{
		Ref:          core.RemoteClaimRef{Issue: r.Issue, Key: r.Key, Epoch: r.Epoch},
		Branch:       r.Branch,
		BaseSHA:      r.BaseSHA,
		TargetBranch: r.TargetBranch,
		Repository:   r.Repository,
		RecordedAt:   r.RecordedAt,
	}
}

// RecordClaim pins the claim-time base for one claim cycle and returns it.
//
// **It must be called, and must have returned, before a remote run for this
// claim can start.** The pin is the only fact in the v2 check that cannot be
// reconstructed after the fact: once the run has pushed, nothing left in the
// world says where the branch started, and a base read afterwards is a base the
// run may have chosen. Everything else the verifier reads is re-readable at any
// time; this is not.
//
// The base is the issue branch's head when the canonical remote already has one
// — work handed off from another daemon, or a previous cycle's — and the
// selected target branch's head otherwise. That is the v1 definition (SPEC
// §6.2 durability, §9.7), taken from the canonical remote instead of from a
// local base repository, so "advanced" keeps meaning "this claim's run added
// commits".
//
// Idempotent within a cycle: a restart between recording and starting the run
// finds the pin, proves the store still holds it, and returns it unchanged. It
// never re-pins a cycle it has already pinned, because by then a run may have
// moved the branch and re-pinning would silently adopt that movement as the
// baseline.
func (m *Mirror) RecordClaim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	return m.RecordClaimRetaining(ctx, ref, nil)
}

// RecordClaimRetaining pins one claim while preserving the refs named by ended
// workspace-cycle obligations. Those refs are no longer verification authority
// once a later epoch's record lands, but they remain the cleanup identity for an
// allocation whose deletion has not been confirmed (#266).
func (m *Mirror) RecordClaimRetaining(
	ctx context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef,
) (core.RemoteClaim, error) {
	if err := validate(ref); err != nil {
		return core.RemoteClaim{}, err
	}
	for _, old := range retained {
		if err := validate(old); err != nil {
			return core.RemoteClaim{}, err
		}
		if old.Key != ref.Key || old.Issue != ref.Issue {
			return core.RemoteClaim{}, fmt.Errorf("%w: retained claim %+v does not belong to %+v",
				ErrClaimRefInvalid, old, ref)
		}
	}
	if err := m.ensure(ctx); err != nil {
		return core.RemoteClaim{}, err
	}
	unlock := m.lockKey(ref.Key)
	defer unlock()

	switch existing, err := m.readRecord(ref.Key); {
	case err != nil && !errors.Is(err, ErrClaimUnrecorded):
		return core.RemoteClaim{}, err
	case err == nil && existing.Issue != ref.Issue:
		return core.RemoteClaim{}, fmt.Errorf("%w: workspace key %s is recorded for issue %s, asked for %s",
			ErrClaimRefInvalid, ref.Key, existing.Issue, ref.Issue)
	case err == nil && existing.Repository != m.repository:
		return core.RemoteClaim{}, fmt.Errorf("%w: issue %s is pinned against %q, this mirror reads %q",
			ErrRepositoryMismatch, ref.Issue, existing.Repository, m.repository)
	case err == nil && existing.Epoch == ref.Epoch:
		claim, err := m.claimLocked(ctx, ref)
		if err != nil {
			return core.RemoteClaim{}, err
		}
		// A previous call can have renamed the record and then failed its
		// directory sync, leaving an apparently complete record behind. Finish
		// that transaction before an idempotent retry authorizes the run, and
		// repeat pruning in case the earlier call stopped there instead.
		if err := m.finishRecord(ctx, ref, retained); err != nil {
			return core.RemoteClaim{}, err
		}
		return claim, nil
	}

	base, target, err := m.pinBase(ctx, ref)
	if err != nil {
		return core.RemoteClaim{}, err
	}
	record := claimRecord{
		Issue:        ref.Issue,
		Key:          ref.Key,
		Epoch:        ref.Epoch,
		Branch:       Branch(ref.Key),
		BaseSHA:      base,
		TargetBranch: target,
		Repository:   m.repository,
		RecordedAt:   m.now().UTC(),
	}
	// The ref is written first and the record second, so the record never names
	// a commit the store does not hold. The other order leaves a crash window in
	// which a claim reads as recorded and its base cannot be resolved — which is
	// ErrClaimPinLost, a park, for a claim that was merely interrupted before it
	// began.
	if err := m.writeRecord(record); err != nil {
		return core.RemoteClaim{}, err
	}
	if err := m.pruneOtherEpochs(ctx, ref, retained); err != nil {
		return core.RemoteClaim{}, err
	}
	m.logger.Info("claim-time base pinned",
		"repository", m.repository, "issue", ref.Issue, "epoch", ref.Epoch,
		"branch", record.Branch, "base", base, "target", target)
	return record.claim(), nil
}

// pinBase resolves the claim-time base on the canonical remote and fetches it
// into this claim's pin ref, returning the commit the store then holds.
//
// The probe and the fetch are separate requests, so the branch can move between
// them. That is ErrRemoteRaced rather than a base of either value: a pin is a
// promise that the base was fixed at a known instant, and a pin taken from a
// moving branch is the one thing it must not be.
func (m *Mirror) pinBase(ctx context.Context, ref core.RemoteClaimRef) (string, string, error) {
	target, targetSHA, err := m.resolveTargetBranch(ctx)
	if err != nil {
		return "", "", err
	}
	branchRef := "refs/heads/" + Branch(ref.Key)
	source := branchRef
	probed, ok, err := m.lsRemote(ctx, branchRef)
	if err != nil {
		return "", "", err
	}
	if !ok {
		source = "refs/heads/" + target
		probed = targetSHA
	}
	fetched, err := m.fetchInto(ctx, source, m.pinRef(ref))
	if err != nil {
		return "", "", err
	}
	if fetched != probed {
		return "", "", fmt.Errorf("%w: %s was %s at the probe and %s at the fetch",
			ErrRemoteRaced, source, probed, fetched)
	}
	return fetched, target, nil
}

func (m *Mirror) resolveTargetBranch(ctx context.Context) (string, string, error) {
	target := m.baseBranch
	if target == "" {
		var err error
		target, err = m.defaultBranch(ctx)
		if err != nil {
			return "", "", err
		}
	}
	if target == "ben" || strings.HasPrefix(target, branchPrefix) {
		return "", "", fmt.Errorf("%w: %q", ErrBaseBranchReserved, target)
	}
	sha, ok, err := m.lsRemote(ctx, "refs/heads/"+target)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrBaseBranchNotFound, target)
	}
	return target, sha, nil
}

// Claim reads back a recorded pin, proving the store still holds it.
//
// Absence is an error and not a zero value, because every caller of this acts on
// the answer: a claim with no pin has no leg 1 to check, and reporting that as
// an empty base would make every candidate descend from nothing.
func (m *Mirror) Claim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	if err := validate(ref); err != nil {
		return core.RemoteClaim{}, err
	}
	if err := m.ensure(ctx); err != nil {
		return core.RemoteClaim{}, err
	}
	unlock := m.lockKey(ref.Key)
	defer unlock()
	return m.claimLocked(ctx, ref)
}

func (m *Mirror) claimLocked(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	record, err := m.readRecord(ref.Key)
	if err != nil {
		return core.RemoteClaim{}, err
	}
	if record.Epoch != ref.Epoch {
		return core.RemoteClaim{}, fmt.Errorf("%w: issue %s is pinned for epoch %d, asked for %d",
			ErrClaimEpochMismatch, ref.Issue, record.Epoch, ref.Epoch)
	}
	if record.Issue != ref.Issue {
		return core.RemoteClaim{}, fmt.Errorf("%w: workspace key %s is recorded for issue %s, asked for %s",
			ErrClaimRefInvalid, ref.Key, record.Issue, ref.Issue)
	}
	if record.Repository != m.repository {
		return core.RemoteClaim{}, fmt.Errorf("%w: issue %s is pinned against %q, this mirror reads %q",
			ErrRepositoryMismatch, ref.Issue, record.Repository, m.repository)
	}
	if record.TargetBranch == "" {
		return core.RemoteClaim{}, fmt.Errorf("%w: issue %s epoch %d", ErrClaimTargetUnrecorded, ref.Issue, ref.Epoch)
	}
	if err := m.provePin(ctx, ref, record.BaseSHA); err != nil {
		return core.RemoteClaim{}, err
	}
	return record.claim(), nil
}

// provePin checks that the pin ref still resolves to the commit the record
// names. Both halves are refusals: a ref that is gone means the object store can
// no longer order anything against the base, and one that moved means something
// outside this package rewrote a pin.
func (m *Mirror) provePin(ctx context.Context, ref core.RemoteClaimRef, want string) error {
	sha, ok, err := m.revParse(ctx, m.pinRef(ref))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s is recorded at %s and the store has no such pin",
			ErrClaimPinLost, m.pinRef(ref), want)
	}
	if sha != want {
		return fmt.Errorf("%w: %s is recorded at %s and the store pins %s",
			ErrClaimPinLost, m.pinRef(ref), want, sha)
	}
	return nil
}

// RemoteFacts reads the daemon-side git evidence for one verification: the
// canonical remote's head for this claim's branch, ordered against the pin.
//
// Nothing here consults the run. The branch is derived from the claim's key, the
// head is read from the canonical remote with the daemon's own credential, and
// the ancestry is computed over objects in a store no run can reach. A sandbox
// that reports success, writes a transcript saying it pushed, or leaves a
// filesystem that looks published changes none of these fields.
//
// Every answer is an observation made now. There is no cached path and no
// "fetched recently enough" — a head observed at the previous tick and a head
// observed at this one are different facts about a branch anyone can move, and
// Fetched is set only when this call reached the remote (SPEC §9.7 fail closed).
func (m *Mirror) RemoteFacts(ctx context.Context, run core.RemoteRunRef) (core.RemotePublishFacts, error) {
	if !run.Complete() {
		return core.RemotePublishFacts{}, fmt.Errorf(
			"%w: a verification names a claim, a run and a verification attempt (got %+v)", ErrClaimRefInvalid, run)
	}
	if err := validate(run.Claim); err != nil {
		return core.RemotePublishFacts{}, err
	}
	if err := m.ensure(ctx); err != nil {
		return core.RemotePublishFacts{}, err
	}

	// Held across the whole observation, so two verifications of one claim
	// cannot interleave their fetches into one ref and read each other's head.
	unlock := m.lockKey(run.Claim.Key)
	defer unlock()

	claim, err := m.claimLocked(ctx, run.Claim)
	if err != nil {
		return core.RemotePublishFacts{}, err
	}

	facts := core.RemotePublishFacts{
		Run:        run,
		Repository: m.repository,
		Branch:     claim.Branch,
		BaseSHA:    claim.BaseSHA,
	}

	branchRef := "refs/heads/" + claim.Branch
	probed, ok, err := m.lsRemote(ctx, branchRef)
	if err != nil {
		return core.RemotePublishFacts{}, err
	}
	if !ok {
		// Absent on the canonical remote. The stale observation ref goes with it:
		// a leftover head from a previous observation would be a remote fact that
		// is no longer one, which is exactly the staleness this check exists to
		// refuse.
		if err := m.deleteRef(ctx, m.headRef(run.Claim)); err != nil {
			return core.RemotePublishFacts{}, err
		}
		facts.Fetched = true
		facts.ObservedAt = m.now().UTC()
		return facts, nil
	}

	head, err := m.fetchInto(ctx, branchRef, m.headRef(run.Claim))
	if err != nil {
		return core.RemotePublishFacts{}, err
	}
	if head != probed {
		return core.RemotePublishFacts{}, fmt.Errorf("%w: %s was %s at the probe and %s at the fetch",
			ErrRemoteRaced, branchRef, probed, head)
	}
	facts.RemoteHead = head
	facts.Fetched = true
	if facts.DescendsBase, err = m.isAncestor(ctx, claim.BaseSHA, head); err != nil {
		return core.RemotePublishFacts{}, err
	}
	facts.ObservedAt = m.now().UTC()
	return facts, nil
}

// Discard removes a claim's pin, its observation ref and its record — what a
// finished claim cycle leaves behind.
//
// Not a verification path, and deliberately tolerant of a store that has already
// forgotten: a cleanup that refuses because there is nothing to clean turns a
// completed claim into an error nobody can resolve.
func (m *Mirror) Discard(ctx context.Context, ref core.RemoteClaimRef) error {
	if err := validate(ref); err != nil {
		return err
	}
	switch present, err := m.present(); {
	case err != nil:
		return err
	case !present:
		return nil
	}
	// Even a missing record or a stale epoch reaches the ref cleanup below.
	// Prove the on-disk store is this repository's before either case is allowed
	// to mutate it; a record's embedded identity does not identify the tree the
	// record currently lives in.
	if err := m.verifyIdentity(ctx); err != nil {
		return err
	}
	unlock := m.lockKey(ref.Key)
	defer unlock()

	record, err := m.readRecord(ref.Key)
	recordAbsent := errors.Is(err, ErrClaimUnrecorded)
	needsRecordSync := recordAbsent
	switch {
	case err != nil && !recordAbsent:
		return err
	case recordAbsent:
		// Absence may be the visible result of an earlier removal whose
		// directory sync failed. Repeat that sync before deleting refs so a
		// crash cannot restore an authoritative record without its pin.
	case err == nil && record.Epoch != ref.Epoch:
		// A delayed cleanup from an earlier cycle owns only that cycle's refs.
		// The key's record now authorizes a later run and must survive.
	case err == nil && record.Issue != ref.Issue:
		return fmt.Errorf("%w: workspace key %s is recorded for issue %s, asked for %s",
			ErrClaimRefInvalid, ref.Key, record.Issue, ref.Issue)
	case err == nil && record.Repository != m.repository:
		return fmt.Errorf("%w: issue %s is pinned against %q, this mirror reads %q",
			ErrRepositoryMismatch, ref.Issue, record.Repository, m.repository)
	case err == nil:
		// Unpublish the record before its refs. A crash may leave harmless orphan
		// refs for the next cleanup, but can never leave an authoritative record
		// naming a pin that cleanup already removed.
		if err := os.Remove(m.recordPath(ref.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("mirror: removing the claim record for %s: %w", ref.Key, err)
		}
		needsRecordSync = true
	}
	if needsRecordSync {
		if err := m.syncDirectory(m.claimsDir); err != nil {
			return fmt.Errorf("mirror: making removal of the claim record for %s durable: %w", ref.Key, err)
		}
	}
	if err := m.deleteRef(ctx, m.pinRef(ref)); err != nil {
		return err
	}
	if err := m.deleteRef(ctx, m.headRef(ref)); err != nil {
		return err
	}
	return nil
}

// pruneOtherEpochs removes this key's refs from every claim cycle but the
// current one, so a store's ref namespace does not grow with the issue's
// history. It runs after the new record has landed: a prune that ran first would
// widen the crash window in which neither epoch has a usable pin.
func (m *Mirror) pruneOtherEpochs(
	ctx context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef,
) error {
	out, err := m.git(ctx, "for-each-ref", "--format=%(refname)", pinRefPrefix, headRefPrefix)
	if err != nil {
		return fmt.Errorf("%w: cannot enumerate the store's claim refs: %v", ErrMirrorState, err)
	}
	keep := map[string]bool{m.pinRef(ref): true, m.headRef(ref): true}
	for _, old := range retained {
		keep[m.pinRef(old)] = true
		keep[m.headRef(old)] = true
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || keep[name] || !strings.HasSuffix(name, "/"+ref.Key) {
			continue
		}
		if err := m.deleteRef(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// finishRecord completes the durable publication transaction for a record that
// is already visible. It is intentionally safe to repeat: that is what lets a
// retry distinguish "rename happened, sync failed" from a fully completed
// RecordClaim without another piece of state that would need the same protocol.
func (m *Mirror) finishRecord(
	ctx context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef,
) error {
	if err := m.syncDirectory(m.claimsDir); err != nil {
		return fmt.Errorf("mirror: making the claim record for %s durable: %w", ref.Key, err)
	}
	return m.pruneOtherEpochs(ctx, ref, retained)
}

func (m *Mirror) recordPath(key string) string {
	return filepath.Join(m.claimsDir, key+".json")
}

// readRecord reads one key's claim record. A record that is absent, unreadable
// or unparseable is ErrClaimUnrecorded only in the first case: the other two are
// a store that cannot be read, and reading them as "no claim" would let a
// verification proceed against no pin at all.
func (m *Mirror) readRecord(key string) (claimRecord, error) {
	raw, err := os.ReadFile(m.recordPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return claimRecord{}, fmt.Errorf("%w: no record for workspace key %s", ErrClaimUnrecorded, key)
		}
		return claimRecord{}, fmt.Errorf("%w: cannot read the claim record for %s: %v", ErrMirrorState, key, err)
	}
	var record claimRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return claimRecord{}, fmt.Errorf("%w: cannot parse the claim record for %s: %v", ErrMirrorState, key, err)
	}
	if record.Key != key || record.Epoch <= 0 || record.BaseSHA == "" {
		return claimRecord{}, fmt.Errorf("%w: the claim record for %s is incomplete", ErrMirrorState, key)
	}
	return record, nil
}

func (m *Mirror) writeRecord(record claimRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("mirror: encoding the claim record for %s: %w", record.Key, err)
	}
	return atomicWrite(m.recordPath(record.Key), append(body, '\n'), m.syncDirectory)
}

// atomicWrite replaces a whole file so that a reader sees either the previous
// contents or the new ones, never a splice of both.
//
// The ordering is internal/state's, for internal/state's reason (and the run
// marker's before it): rename publishes a *directory entry*, not the bytes
// behind it, so a rename before the sync can name a file whose contents have not
// landed. After a crash that reads as a truncated record — which here is a
// recorded claim whose base nobody can resolve, and a park.
func atomicWrite(path string, body []byte, syncDirectory func(string) error) error {
	dir := filepath.Dir(path)
	if err := mkdirAllSynced(dir, 0o700, syncDirectory); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // no-op once the rename succeeds
	if _, err := tmp.Write(body); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the one that matters
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // as above
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// syncDir makes a directory entry's creation durable. Syncing the file is not
// enough: the entry that names it lives in the directory, and a crash can lose
// the name while keeping the data.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read-only handle; Sync reported what mattered
	return d.Sync()
}

// syncTree flushes files before the directories that name them, from the
// leaves back to root. It is used only for first-time store creation: once the
// tree is renamed into place, syncing its parent can then publish a complete
// initialized store rather than merely a durable top-level name.
func syncTree(root string, syncDirectory func(string) error) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAllSynced is MkdirAll with a durability account: every newly created
// directory has the parent entry naming it synced, from the highest missing
// ancestor down.
func mkdirAllSynced(dir string, perm os.FileMode, syncDirectory func(string) error) error {
	dir = filepath.Clean(dir)
	var missing []string

walk:
	for cursor := dir; ; cursor = filepath.Dir(cursor) {
		info, err := os.Stat(cursor)
		switch {
		case err == nil:
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", cursor)
			}
			break walk
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		missing = append(missing, cursor)
		if parent := filepath.Dir(cursor); parent == cursor {
			return fmt.Errorf("no existing ancestor for %s", dir)
		}
	}

	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		info, err := os.Stat(missing[i])
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", missing[i])
		}
		if err := syncDirectory(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

// lockKey serializes one claim's record and refs against themselves.
func (m *Mirror) lockKey(key string) (unlock func()) {
	actual, _ := m.locks.claims.LoadOrStore(key, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
