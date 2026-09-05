package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type HookID string

// HookKey is the stable storage key for one hook firing.
type HookKey struct {
	Claim Claim
	ID    HookID
}

func (k HookKey) Valid() bool { return k.Claim.Valid() && k.ID != "" }
func (k HookKey) String() string {
	return k.Claim.String() + "/hook/" + string(k.ID)
}

// HookRef names one exact hook firing in one exact sandbox.
type HookRef struct {
	Identity      Identity  `json:"identity"`
	ID            HookID    `json:"id"`
	Phase         HookPhase `json:"phase"`
	Attempt       int       `json:"attempt"`
	RequestDigest string    `json:"request_digest"`
}

func (r HookRef) Key() HookKey { return HookKey{Claim: r.Identity.Claim, ID: r.ID} }
func (r HookRef) Complete() bool {
	return r.Identity.Complete() && r.ID != "" && validHookPhase(r.Phase) &&
		r.Attempt >= 0 && r.RequestDigest != ""
}

// HookSpec is the immutable backend request covered by HookRef.RequestDigest.
type HookSpec struct {
	Identity Identity
	Phase    HookPhase
	Attempt  int
	Script   string
	Argv     []string
	Git      GitScope
	Timeout  time.Duration
}

func HookRequestDigest(spec HookSpec) (string, error) {
	digest, err := marshalRequestDigest(hookRequestDigestPayload(spec))
	if err != nil {
		return "", fmt.Errorf("remote: encoding hook request: %w", err)
	}
	return digest, nil
}

type HookState uint8

const (
	HookStateUnknown HookState = iota
	HookStateRunning
	HookStateFinished
)

type HookStatus struct {
	State  HookState
	Result HookResult
	Domain DomainState
}

// HookExec is the durable hook-process seam. Start is idempotent for the exact
// ref and request, so an unknown response is resolved by replaying StartScript;
// Status observes that exact firing, and Wait blocks until it is finished or
// ctx ends.
type HookExec interface {
	StartScript(ctx context.Context, ref HookRef, spec HookSpec) (HookStatus, error)
	StatusScript(ctx context.Context, ref HookRef) (HookStatus, error)
	WaitScript(ctx context.Context, ref HookRef) (HookStatus, error)
}

type HookRecord struct {
	Version    int         `json:"version"`
	Ref        HookRef     `json:"ref"`
	Dispatched bool        `json:"dispatched"`
	Result     *HookResult `json:"result,omitempty"`
}

const HookRecordVersion = 1

type HookStore interface {
	LoadHook(key HookKey) (HookRecord, error)
	SaveHook(record HookRecord) error
	DeleteHook(key HookKey) error
}

func EncodeHookRecord(r HookRecord) ([]byte, error) {
	if r.Version == 0 {
		r.Version = HookRecordVersion
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("remote: encoding hook record %s: %w", r.Ref.Key(), err)
	}
	return append(body, '\n'), nil
}

func DecodeHookRecord(body []byte) (HookRecord, error) {
	var r HookRecord
	if err := json.Unmarshal(body, &r); err != nil {
		return HookRecord{}, fmt.Errorf("remote: decoding hook record: %w", err)
	}
	if r.Version > HookRecordVersion {
		return HookRecord{}, fmt.Errorf("remote: hook record is version %d and this binary understands %d", r.Version, HookRecordVersion)
	}
	return r, nil
}

type HookDirStore struct{ root string }

func NewHookDirStore(root string) *HookDirStore { return &HookDirStore{root: root} }

func (s *HookDirStore) Path(key HookKey) string {
	name := claimFilename(key.Claim) + "-hook-" + shortDigest(key.String()) + ".json"
	return filepath.Join(s.root, "hooks", name)
}

func (s *HookDirStore) LoadHook(key HookKey) (HookRecord, error) {
	body, err := os.ReadFile(s.Path(key))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return HookRecord{}, fmt.Errorf("%w: %s", ErrNoRecord, key)
	case err != nil:
		return HookRecord{}, fmt.Errorf("remote: reading hook record %s: %w", key, err)
	}
	r, err := DecodeHookRecord(body)
	if err != nil {
		return HookRecord{}, err
	}
	if r.Ref.Key() != key {
		return HookRecord{}, fmt.Errorf("%w: hook file %s names %s", ErrHookMismatch, s.Path(key), r.Ref.Key())
	}
	return r, nil
}

func (s *HookDirStore) SaveHook(r HookRecord) error {
	body, err := EncodeHookRecord(r)
	if err != nil {
		return err
	}
	return replaceFile(s.Path(r.Ref.Key()), body)
}

func (s *HookDirStore) DeleteHook(key HookKey) error {
	err := os.Remove(s.Path(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remote: removing hook record %s: %w", key, err)
	}
	return nil
}

type HookJournal struct {
	store HookStore
	rec   HookRecord
}

func OpenHookJournal(store HookStore, key HookKey) (*HookJournal, error) {
	if store == nil || !key.Valid() {
		return nil, fmt.Errorf("%w: incomplete hook key", ErrHookMismatch)
	}
	r, err := store.LoadHook(key)
	if err != nil {
		return nil, err
	}
	if !r.Ref.Complete() {
		return nil, fmt.Errorf("%w: incomplete hook reference %s", ErrHookMismatch, key)
	}
	return &HookJournal{store: store, rec: r}, nil
}

func ReserveHook(ctx context.Context, store HookStore, ref HookRef) (*HookJournal, error) {
	if store == nil || !ref.Complete() {
		return nil, fmt.Errorf("%w: incomplete hook reference", ErrHookMismatch)
	}
	j := &HookJournal{store: store, rec: HookRecord{Version: HookRecordVersion, Ref: ref}}
	if err := j.save(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *HookJournal) Record() HookRecord {
	r := j.rec
	if r.Result != nil {
		result := *r.Result
		r.Result = &result
	}
	return r
}

func (j *HookJournal) Resume() (HookRef, bool, *HookResult) {
	r := j.Record()
	return r.Ref, r.Dispatched, r.Result
}

func (j *HookJournal) Dispatch(ctx context.Context, start func(context.Context, HookRef) (HookStatus, error)) (HookStatus, error) {
	if j.rec.Dispatched {
		return HookStatus{}, fmt.Errorf("%w: hook %s", ErrAlreadyStarted, j.rec.Ref.Key())
	}
	j.rec.Dispatched = true
	if err := j.save(ctx); err != nil {
		j.rec.Dispatched = false
		return HookStatus{}, err
	}
	return start(ctx, j.rec.Ref)
}

func (j *HookJournal) Complete(ctx context.Context, result HookResult) error {
	previous := j.rec.Result
	j.rec.Result = &result
	if err := j.save(ctx); err != nil {
		j.rec.Result = previous
		return err
	}
	return nil
}

func (j *HookJournal) save(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.store.SaveHook(j.rec)
}

func (s HookState) String() string {
	switch s {
	case HookStateUnknown:
		return "unknown"
	case HookStateRunning:
		return "running"
	case HookStateFinished:
		return "finished"
	default:
		return "HookState(" + strconv.Itoa(int(s)) + ")"
	}
}
