package localdomain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type cleanupTarget interface {
	// exited is ignored for startup residue, which has no live handle. For a
	// newly launched attempt it must positively observe supervisor exit.
	exited() (bool, error)
	empty() (bool, error)
	remove(context.Context, int) (bool, error)
	close()
}

type cleanupEntry struct {
	key               string
	target            cleanupTarget
	requireSupervisor bool
	failures          int
}

type cleanupQueue struct {
	timings Timings

	mu       sync.Mutex
	entries  map[string]*cleanupEntry
	degraded error
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	close    sync.Once
}

func newCleanupQueue(timings Timings) *cleanupQueue {
	q := &cleanupQueue{
		timings: timings.withDefaults(),
		entries: make(map[string]*cleanupEntry),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *cleanupQueue) register(key string, target cleanupTarget, requireSupervisor bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.degraded != nil {
		return errors.Join(ErrCleanupDegraded, q.degraded)
	}
	if _, exists := q.entries[key]; exists {
		return fmt.Errorf("cleanup target %q already registered", key)
	}
	q.entries[key] = &cleanupEntry{key: key, target: target, requireSupervisor: requireSupervisor}
	q.signal()
	return nil
}

func (q *cleanupQueue) health() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.degraded == nil {
		return nil
	}
	return errors.Join(ErrCleanupDegraded, q.degraded)
}

func (q *cleanupQueue) run() {
	defer close(q.done)
	timer := time.NewTimer(q.timings.CleanupRetry)
	defer timer.Stop()
	for {
		select {
		case <-q.wake:
		case <-timer.C:
		case <-q.stop:
			q.closeTargets()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), q.timings.CleanupPass)
		q.pass(ctx)
		cancel()
		timer.Reset(q.timings.CleanupRetry)
	}
}

func (q *cleanupQueue) pass(ctx context.Context) {
	q.mu.Lock()
	entries := make([]*cleanupEntry, 0, len(q.entries))
	for _, entry := range q.entries {
		entries = append(entries, entry)
	}
	q.mu.Unlock()

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		ready, err := q.cleanupReady(entry)
		if err != nil {
			q.failed(entry, err)
			continue
		}
		if !ready {
			q.healthy(entry)
			continue
		}
		removed, err := entry.target.remove(ctx, q.timings.CleanupNodes)
		if err != nil {
			q.failed(entry, err)
			continue
		}
		if !removed {
			q.failed(entry, fmt.Errorf("cleanup target %q remained after bounded pass", entry.key))
			continue
		}
		q.removed(entry)
	}
}

func (q *cleanupQueue) cleanupReady(entry *cleanupEntry) (bool, error) {
	if entry.requireSupervisor {
		exited, err := entry.target.exited()
		if err != nil || !exited {
			return false, err
		}
	}
	empty, err := entry.target.empty()
	if err != nil || !empty {
		return false, err
	}
	return true, nil
}

func (q *cleanupQueue) healthy(entry *cleanupEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if current := q.entries[entry.key]; current == entry {
		entry.failures = 0
	}
}

func (q *cleanupQueue) failed(entry *cleanupEntry, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if current := q.entries[entry.key]; current != entry {
		return
	}
	entry.failures++
	if entry.failures >= q.timings.CleanupFailures && q.degraded == nil {
		q.degraded = fmt.Errorf("target %q failed %d consecutive cleanup passes: %w",
			entry.key, entry.failures, err)
	}
}

func (q *cleanupQueue) removed(entry *cleanupEntry) {
	q.mu.Lock()
	if current := q.entries[entry.key]; current != entry {
		q.mu.Unlock()
		return
	}
	delete(q.entries, entry.key)
	q.mu.Unlock()
	entry.target.close()
}

func (q *cleanupQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *cleanupQueue) closeTargets() {
	q.mu.Lock()
	entries := q.entries
	q.entries = make(map[string]*cleanupEntry)
	q.mu.Unlock()
	for _, entry := range entries {
		entry.target.close()
	}
}

func (q *cleanupQueue) Close() {
	q.close.Do(func() { close(q.stop) })
	<-q.done
}
