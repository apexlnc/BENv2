package localdomain

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeCleanupTarget struct {
	mu             sync.Mutex
	supervisorGone bool
	isEmpty        bool
	exitErr        error
	emptyErr       error
	removeErr      error
	remain         bool
	removeCalls    int
	closed         bool
	removed        chan struct{}
}

func (f *fakeCleanupTarget) exited() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.supervisorGone, f.exitErr
}

func (f *fakeCleanupTarget) empty() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isEmpty, f.emptyErr
}

func (f *fakeCleanupTarget) remove(context.Context, int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	if f.removeErr != nil {
		return false, f.removeErr
	}
	if f.remain {
		return false, nil
	}
	select {
	case <-f.removed:
	default:
		close(f.removed)
	}
	return true, nil
}

func (f *fakeCleanupTarget) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func cleanupTestTimings() Timings {
	return Timings{
		CleanupRetry:    2 * time.Millisecond,
		CleanupPass:     50 * time.Millisecond,
		CleanupNodes:    8,
		CleanupFailures: 3,
	}
}

func TestJanitorOwnsTargetAfterCallerDropsIt(t *testing.T) {
	q := newCleanupQueue(cleanupTestTimings())
	defer q.Close()
	target := &fakeCleanupTarget{supervisorGone: true, isEmpty: true, removed: make(chan struct{})}
	if err := q.register("run-a", target, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-target.removed:
	case <-time.After(time.Second):
		t.Fatal("janitor did not remove an eligible target")
	}
	target.mu.Lock()
	closed := target.closed
	target.mu.Unlock()
	if !closed {
		t.Fatal("removed target was not closed")
	}
}

func TestJanitorRequiresExitAndRecursiveEmpty(t *testing.T) {
	q := newCleanupQueue(cleanupTestTimings())
	defer q.Close()
	target := &fakeCleanupTarget{removed: make(chan struct{})}
	if err := q.register("run-b", target, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	target.mu.Lock()
	if target.removeCalls != 0 {
		t.Fatalf("remove calls while supervisor live = %d", target.removeCalls)
	}
	target.supervisorGone = true
	target.mu.Unlock()
	time.Sleep(15 * time.Millisecond)
	target.mu.Lock()
	if target.removeCalls != 0 {
		t.Fatalf("remove calls while populated = %d", target.removeCalls)
	}
	target.isEmpty = true
	target.mu.Unlock()
	q.signal()
	select {
	case <-target.removed:
	case <-time.After(time.Second):
		t.Fatal("janitor did not remove target after both facts held")
	}
}

func TestStartupResidueNeedsOnlyRecursiveEmpty(t *testing.T) {
	q := newCleanupQueue(cleanupTestTimings())
	defer q.Close()
	target := &fakeCleanupTarget{isEmpty: true, removed: make(chan struct{})}
	if err := q.register("run-c", target, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-target.removed:
	case <-time.After(time.Second):
		t.Fatal("empty startup residue was not removed")
	}
}

func TestJanitorRetainsOnObservationErrorsAndRepopulation(t *testing.T) {
	sentinel := errors.New("observation torn")
	for _, tc := range []struct {
		name   string
		target *fakeCleanupTarget
		clear  func(*fakeCleanupTarget)
	}{
		{
			name:   "supervisor observation error",
			target: &fakeCleanupTarget{supervisorGone: true, isEmpty: true, exitErr: sentinel, removed: make(chan struct{})},
			clear:  func(target *fakeCleanupTarget) { target.exitErr = nil },
		},
		{
			name:   "population observation error",
			target: &fakeCleanupTarget{supervisorGone: true, isEmpty: true, emptyErr: sentinel, removed: make(chan struct{})},
			clear:  func(target *fakeCleanupTarget) { target.emptyErr = nil },
		},
		{
			name:   "recursive population",
			target: &fakeCleanupTarget{supervisorGone: true, isEmpty: false, removed: make(chan struct{})},
			clear:  func(target *fakeCleanupTarget) { target.isEmpty = true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timings := cleanupTestTimings()
			timings.CleanupFailures = 100
			q := newCleanupQueue(timings)
			defer q.Close()
			if err := q.register("run-retained", tc.target, true); err != nil {
				t.Fatal(err)
			}
			time.Sleep(4 * timings.CleanupRetry)
			tc.target.mu.Lock()
			if tc.target.removeCalls != 0 {
				t.Fatalf("remove calls before positive cleanup predicate = %d", tc.target.removeCalls)
			}
			tc.clear(tc.target)
			tc.target.mu.Unlock()
			q.signal()
			select {
			case <-tc.target.removed:
			case <-time.After(time.Second):
				t.Fatal("janitor did not retry retained target")
			}
		})
	}
}

func TestJanitorRetriesIfTargetRepopulatesDuringRemoval(t *testing.T) {
	timings := cleanupTestTimings()
	timings.CleanupFailures = 100
	q := newCleanupQueue(timings)
	defer q.Close()
	target := &fakeCleanupTarget{
		supervisorGone: true,
		isEmpty:        true,
		remain:         true,
		removed:        make(chan struct{}),
	}
	if err := q.register("run-repopulated", target, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		target.mu.Lock()
		calls := target.removeCalls
		target.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("janitor did not reach removal")
		}
		time.Sleep(time.Millisecond)
	}
	target.mu.Lock()
	target.isEmpty = false
	target.remain = false
	target.mu.Unlock()
	q.signal()
	time.Sleep(4 * timings.CleanupRetry)
	select {
	case <-target.removed:
		t.Fatal("repopulated target was removed")
	default:
	}
	target.mu.Lock()
	target.isEmpty = true
	target.mu.Unlock()
	q.signal()
	select {
	case <-target.removed:
	case <-time.After(time.Second):
		t.Fatal("janitor did not remove target after it became empty again")
	}
}

func TestPersistentCleanupFailureDegradesFutureRegistration(t *testing.T) {
	sentinel := errors.New("rmdir busy")
	q := newCleanupQueue(cleanupTestTimings())
	defer q.Close()
	target := &fakeCleanupTarget{
		supervisorGone: true,
		isEmpty:        true,
		removeErr:      sentinel,
		removed:        make(chan struct{}),
	}
	if err := q.register("run-d", target, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for q.health() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := q.health(); !errors.Is(err, ErrCleanupDegraded) || !errors.Is(err, sentinel) {
		t.Fatalf("health = %v, want cleanup degradation wrapping cause", err)
	}
	other := &fakeCleanupTarget{removed: make(chan struct{})}
	if err := q.register("run-e", other, true); !errors.Is(err, ErrCleanupDegraded) {
		t.Fatalf("register error = %v, want ErrCleanupDegraded", err)
	}
}
