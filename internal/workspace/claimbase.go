package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The claim-base record is the one local authority for which assignment epoch
// a verification base belongs to. Git refs below are reachability roots only:
// the whole epoch/base/target state changes with one durable rename of this record.
const (
	claimBaseDirName   = "claims"
	claimBaseVersion   = 2
	legacyClaimVersion = 1
	claimBasePending   = "pending"
	claimBasePinned    = "pinned"
	claimBaseRefPrefix = "refs/ben/claim-base/"
)

type claimBaseFile struct {
	Version              int    `json:"version"`
	State                string `json:"state"`
	Epoch                int64  `json:"epoch"`
	BaseSHA              string `json:"base_sha,omitempty"`
	TargetBranch         string `json:"target_branch,omitempty"`
	OutgoingEpoch        int64  `json:"outgoing_epoch,omitempty"`
	OutgoingBaseSHA      string `json:"outgoing_base_sha,omitempty"`
	OutgoingTargetBranch string `json:"outgoing_target_branch,omitempty"`
}

// BeginClaimBase durably records the pending intent for one tracker-native
// claim epoch. It is idempotent for the same pending or pinned epoch. A pending
// intent for another epoch is not overwritten: that shape needs a human rather
// than a guess about which assignment is current (SPEC §6.2).
func (p *Provider) BeginClaimBase(ctx context.Context, issue core.Issue, epoch int64) error {
	if epoch <= 0 {
		return fmt.Errorf("%w: claim epoch must be positive", ErrClaimEpoch)
	}
	key, err := claimBaseKey(issue)
	if err != nil {
		return err
	}

	unlock := p.lock(key)
	defer unlock()

	current, legacy, err := p.readClaimBaseForTransitionLocked(ctx, key)
	if err != nil {
		return err
	}
	switch current.State {
	case core.ClaimBasePending:
		if current.Epoch != epoch {
			return fmt.Errorf("%w: workspace %s is pending for epoch %d, not %d",
				ErrClaimBaseState, key, current.Epoch, epoch)
		}
		if legacy {
			return fmt.Errorf("%w: workspace %s is pending for epoch %d", ErrClaimTargetUnrecorded, key, epoch)
		}
		return nil
	case core.ClaimBasePinned:
		if current.Epoch == epoch {
			if legacy {
				return fmt.Errorf("%w: workspace %s is pinned for epoch %d", ErrClaimTargetUnrecorded, key, epoch)
			}
			return nil
		}
		return p.writeClaimBaseLocked(key, claimBaseFile{
			Version:              claimBaseVersion,
			State:                claimBasePending,
			Epoch:                epoch,
			OutgoingEpoch:        current.Epoch,
			OutgoingBaseSHA:      current.BaseSHA,
			OutgoingTargetBranch: current.TargetBranch,
		})
	case core.ClaimBaseAbsent:
		outgoing, err := p.legacyOutgoingBaseLocked(ctx, key)
		if err != nil {
			return err
		}
		return p.writeClaimBaseLocked(key, claimBaseFile{
			Version:         claimBaseVersion,
			State:           claimBasePending,
			Epoch:           epoch,
			OutgoingBaseSHA: outgoing,
		})
	default:
		return fmt.Errorf("%w: workspace %s has non-authorizing claim-base state %s",
			ErrClaimBaseState, key, current.State)
	}
}

// ClaimBase reads the closed provider-owned state without preparing, fetching,
// or running hooks. Malformed state and a record whose reachability ref no
// longer agrees are errors rather than a convenient absent reading.
func (p *Provider) ClaimBase(ctx context.Context, issue core.Issue) (core.ClaimBase, error) {
	key, err := claimBaseKey(issue)
	if err != nil {
		return core.ClaimBase{}, err
	}
	unlock := p.lock(key)
	defer unlock()
	return p.readClaimBaseLocked(ctx, key)
}

// AbandonPendingClaimBase rolls an unfinished epoch back only after its claim
// has been ordered to end with the workspace quiet. A prior pinned epoch is
// restored atomically; a genuinely fresh or legacy transition becomes absent,
// leaving any legacy reachability ref for the next BeginClaimBase to observe.
// Pinned state is retained unchanged (SPEC §6.2, §9.8).
func (p *Provider) AbandonPendingClaimBase(ctx context.Context, issue core.Issue) error {
	key, err := claimBaseKey(issue)
	if err != nil {
		return err
	}

	unlock := p.lock(key)
	defer unlock()

	current, err := p.readClaimBaseLocked(ctx, key)
	if err != nil {
		return err
	}
	if current.State != core.ClaimBasePending {
		return nil
	}

	// pinClaimBaseLocked may have published the reachability root before its
	// atomic record write failed. Pending never authorizes that ref, so remove
	// the uncommitted root before publishing the rollback.
	present, err := p.baseRepoPresent()
	if err != nil {
		return err
	}
	if present {
		if _, err := p.baseGit(ctx, "update-ref", "-d", claimBaseRef(key, current.Epoch)); err != nil {
			return fmt.Errorf("%w: removing abandoned claim-base ref: %v", ErrClaimBaseState, err)
		}
	}

	if current.OutgoingEpoch > 0 {
		version := claimBaseVersion
		if current.OutgoingTargetBranch == "" {
			version = legacyClaimVersion
		}
		return p.writeClaimBaseLocked(key, claimBaseFile{
			Version:      version,
			State:        claimBasePinned,
			Epoch:        current.OutgoingEpoch,
			BaseSHA:      current.OutgoingBaseSHA,
			TargetBranch: current.OutgoingTargetBranch,
		})
	}
	return p.removeClaimBaseLocked(key)
}

func claimBaseKey(issue core.Issue) (string, error) {
	if issue.Identifier == "" {
		return "", fmt.Errorf("%w: issue identifier is empty", ErrPathEscape)
	}
	return Key(issue.Identifier), nil
}

func (p *Provider) claimBaseDir() string {
	return filepath.Join(p.wfDir, claimBaseDirName)
}

func (p *Provider) claimBasePath(key string) string {
	return filepath.Join(p.claimBaseDir(), key+".json")
}

func claimBaseRef(key string, epoch int64) string {
	return fmt.Sprintf("%s%s/%d", claimBaseRefPrefix, key, epoch)
}

func (p *Provider) readClaimBaseLocked(ctx context.Context, key string) (core.ClaimBase, error) {
	state, legacy, err := p.readClaimBaseForTransitionLocked(ctx, key)
	if err != nil {
		return core.ClaimBase{}, err
	}
	if legacy {
		return state, fmt.Errorf("%w: workspace %s", ErrClaimTargetUnrecorded, key)
	}
	return state, nil
}

// readClaimBaseForTransitionLocked is the one tolerant read of a pre-#152
// record. BeginClaimBase may carry its base into a later epoch; every public
// and same-epoch consumer goes through readClaimBaseLocked and refuses it.
func (p *Provider) readClaimBaseForTransitionLocked(ctx context.Context, key string) (core.ClaimBase, bool, error) {
	path := p.claimBasePath(key)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, lerr := os.Lstat(path); lerr == nil {
			return core.ClaimBase{}, false, fmt.Errorf("%w: claim-base record %s exists but cannot be read",
				ErrClaimBaseState, path)
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return core.ClaimBase{}, false, fmt.Errorf("%w: inspecting claim-base record %s: %v",
				ErrClaimBaseState, path, lerr)
		}
		if err := p.validateClaimBaseDirForAbsence(); err != nil {
			return core.ClaimBase{}, false, err
		}
		return core.ClaimBase{State: core.ClaimBaseAbsent}, false, nil
	case err != nil:
		return core.ClaimBase{}, false, fmt.Errorf("%w: reading claim-base record %s: %v",
			ErrClaimBaseState, path, err)
	}

	var disk claimBaseFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&disk); err != nil {
		return core.ClaimBase{}, false, fmt.Errorf("%w: decoding claim-base record %s: %v",
			ErrClaimBaseState, path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return core.ClaimBase{}, false, fmt.Errorf("%w: decoding claim-base record %s: %v",
			ErrClaimBaseState, path, err)
	}

	state, legacy, err := projectClaimBase(disk)
	if err != nil {
		return core.ClaimBase{}, false, fmt.Errorf("%w: claim-base record %s: %v",
			ErrClaimBaseState, path, err)
	}
	if err := p.validateClaimBaseRefLocked(ctx, key, state); err != nil {
		return core.ClaimBase{}, false, err
	}
	return state, legacy, nil
}

func projectClaimBase(disk claimBaseFile) (core.ClaimBase, bool, error) {
	legacy := disk.Version == legacyClaimVersion
	if disk.Version != claimBaseVersion && !legacy {
		return core.ClaimBase{}, false, fmt.Errorf("unsupported version %d", disk.Version)
	}
	if disk.Epoch <= 0 {
		return core.ClaimBase{}, false, errors.New("epoch is not positive")
	}
	if legacy && (disk.TargetBranch != "" || disk.OutgoingTargetBranch != "") {
		return core.ClaimBase{}, false, errors.New("legacy record carries target-branch fields")
	}
	switch disk.State {
	case claimBasePending:
		if disk.BaseSHA != "" {
			return core.ClaimBase{}, false, errors.New("pending record carries a pinned base")
		}
		if disk.OutgoingBaseSHA == "" && disk.OutgoingEpoch != 0 {
			return core.ClaimBase{}, false, errors.New("pending record carries an outgoing epoch without a base")
		}
		if disk.OutgoingEpoch < 0 {
			return core.ClaimBase{}, false, errors.New("pending record carries a negative outgoing epoch")
		}
		if disk.OutgoingTargetBranch != "" && disk.OutgoingBaseSHA == "" {
			return core.ClaimBase{}, false, errors.New("pending record carries an outgoing target without a base")
		}
		return core.ClaimBase{
			State:                core.ClaimBasePending,
			Epoch:                disk.Epoch,
			TargetBranch:         disk.TargetBranch,
			OutgoingEpoch:        disk.OutgoingEpoch,
			OutgoingBaseSHA:      disk.OutgoingBaseSHA,
			OutgoingTargetBranch: disk.OutgoingTargetBranch,
		}, legacy, nil
	case claimBasePinned:
		if disk.BaseSHA == "" {
			return core.ClaimBase{}, false, errors.New("pinned record carries no base")
		}
		if !legacy && disk.TargetBranch == "" {
			return core.ClaimBase{}, false, errors.New("pinned record carries no target branch")
		}
		if disk.OutgoingEpoch != 0 || disk.OutgoingBaseSHA != "" || disk.OutgoingTargetBranch != "" {
			return core.ClaimBase{}, false, errors.New("pinned record carries transitional outgoing state")
		}
		return core.ClaimBase{State: core.ClaimBasePinned, Epoch: disk.Epoch, BaseSHA: disk.BaseSHA, TargetBranch: disk.TargetBranch}, legacy, nil
	default:
		return core.ClaimBase{}, false, fmt.Errorf("unknown state %q", disk.State)
	}
}

func (p *Provider) validateClaimBaseRefLocked(ctx context.Context, key string, state core.ClaimBase) error {
	var ref, want string
	switch state.State {
	case core.ClaimBasePinned:
		ref, want = claimBaseRef(key, state.Epoch), state.BaseSHA
	case core.ClaimBasePending:
		if state.OutgoingBaseSHA == "" {
			return nil
		}
		want = state.OutgoingBaseSHA
		if state.OutgoingEpoch > 0 {
			ref = claimBaseRef(key, state.OutgoingEpoch)
		} else {
			ref = baseRefPrefix + key
		}
	default:
		return nil
	}
	got, ok, err := p.revParse(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w: reading reachability ref %s: %v", ErrClaimBaseState, ref, err)
	}
	if !ok || got != want {
		return fmt.Errorf("%w: reachability ref %s is %q, want %q",
			ErrClaimBaseState, ref, got, want)
	}
	return nil
}

func (p *Provider) validateClaimBaseDirForAbsence() error {
	dir := p.claimBaseDir()
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w: inspecting claim-base store %s: %v", ErrClaimBaseState, dir, err)
	case !info.IsDir():
		return fmt.Errorf("%w: claim-base store %s is not a directory", ErrClaimBaseState, dir)
	default:
		return nil
	}
}

// legacyOutgoingBaseLocked admits refs/ben/base/<key> only as the outgoing
// comparison fact of a newly begun epoch. It never projects that unscoped ref
// as a current pinned claim (SPEC §9.10 rollout rule).
func (p *Provider) legacyOutgoingBaseLocked(ctx context.Context, key string) (string, error) {
	info, err := os.Stat(p.baseDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("%w: inspecting base repository for a legacy pin: %v", ErrClaimBaseState, err)
	case !info.IsDir():
		return "", fmt.Errorf("%w: base repository is not a directory", ErrBaseRepoState)
	}
	sha, ok, err := p.revParse(ctx, baseRefPrefix+key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return sha, nil
}

func (p *Provider) pinClaimBaseLocked(ctx context.Context, key string, pending core.ClaimBase, baseSHA, targetBranch string) error {
	if pending.State != core.ClaimBasePending || pending.Epoch <= 0 || baseSHA == "" || targetBranch == "" {
		return fmt.Errorf("%w: invalid pending-to-pinned transition for workspace %s", ErrClaimBaseState, key)
	}
	ref := claimBaseRef(key, pending.Epoch)
	if _, err := p.baseGit(ctx, "update-ref", ref, baseSHA); err != nil {
		return fmt.Errorf("%w: retaining claim base at %s: %v", ErrClaimBaseState, ref, err)
	}
	return p.writeClaimBaseLocked(key, claimBaseFile{
		Version:      claimBaseVersion,
		State:        claimBasePinned,
		Epoch:        pending.Epoch,
		BaseSHA:      baseSHA,
		TargetBranch: targetBranch,
	})
}

// writeClaimBaseLocked publishes a whole state with one rename. The temp file
// is synced before its name becomes authoritative and the directory after, so
// a successful return survives a crash without exposing a torn epoch/base/target tuple.
func (p *Provider) writeClaimBaseLocked(key string, state claimBaseFile) error {
	if key == "" {
		return fmt.Errorf("%w: empty workspace key", ErrPathEscape)
	}
	dir := p.claimBaseDir()
	// BeginClaimBase precedes the first Prepare, so unlike runs/ the workflow
	// subtree may not exist yet. Make each directory entry durable from the first
	// missing ancestor down; syncing only claims/ would let a power loss erase
	// its newly created workflow parent after this function returned success.
	if err := mkdirAllSynced(p.wfDir, 0o755); err != nil {
		return fmt.Errorf("%w: creating workflow directory for claim-base store: %v", ErrClaimBaseState, err)
	}
	if err := mkdirAllSynced(dir, 0o700); err != nil {
		return fmt.Errorf("%w: creating claim-base store: %v", ErrClaimBaseState, err)
	}

	body, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("%w: encoding claim-base record: %v", ErrClaimBaseState, err)
	}
	tmp, err := os.CreateTemp(dir, ".claim-")
	if err != nil {
		return fmt.Errorf("%w: creating claim-base record: %v", ErrClaimBaseState, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: writing claim-base record: %v", ErrClaimBaseState, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: syncing claim-base record: %v", ErrClaimBaseState, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: closing claim-base record: %v", ErrClaimBaseState, err)
	}
	if err := os.Rename(tmp.Name(), p.claimBasePath(key)); err != nil {
		return fmt.Errorf("%w: publishing claim-base record: %v", ErrClaimBaseState, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: syncing claim-base store: %v", ErrClaimBaseState, err)
	}
	return nil
}

// removeClaimBaseLocked durably publishes ClaimBaseAbsent. The caller has
// already read and validated the record under the issue lock.
func (p *Provider) removeClaimBaseLocked(key string) error {
	path := p.claimBasePath(key)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: removing claim-base record %s: %v", ErrClaimBaseState, path, err)
	}
	if err := syncDir(p.claimBaseDir()); err != nil {
		return fmt.Errorf("%w: syncing claim-base store after removal: %v", ErrClaimBaseState, err)
	}
	return nil
}

// mkdirAllSynced is MkdirAll with a durability account. Every directory that
// was absent at observation has the parent entry naming it synced, from the
// highest missing ancestor down. It also refuses a non-directory in the
// created chain instead of reporting a durable store through one. Stat follows
// configured root aliases just as the rest of the workspace provider does.
func mkdirAllSynced(dir string, perm os.FileMode) error {
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
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}
