package remotetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Consumer is an in-memory durable-consumer stand-in. It stores encoded
// consumptions so idempotence compares the same representation a disk-backed
// implementation would retain.
type Consumer struct {
	mu       sync.Mutex
	seen     map[string][]byte
	terminal map[string]bool
	entries  []remote.Consumption
	byRef    map[string][]remote.Consumption
	fault    func(remote.Consumption) error
	recover  error
	after    func(remote.Consumption)
}

func NewConsumer() *Consumer {
	return &Consumer{
		seen: map[string][]byte{}, terminal: map[string]bool{},
		byRef: map[string][]remote.Consumption{},
	}
}

func (c *Consumer) Commit(ctx context.Context, ref remote.ProcessRef, item remote.Consumption) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	body, err := json.Marshal(item)
	if err != nil {
		return false, fmt.Errorf("remotetest: encoding consumption: %w", err)
	}
	key := ref.String() + "\x00" + item.ID
	c.mu.Lock()
	if c.fault != nil {
		if err := c.fault(item); err != nil {
			c.mu.Unlock()
			return false, err
		}
	}
	if previous, ok := c.seen[key]; ok {
		if !bytes.Equal(previous, body) {
			c.mu.Unlock()
			return false, fmt.Errorf("%w: consumption %s", remote.ErrEventConflict, item.ID)
		}
		c.mu.Unlock()
		return false, nil
	}
	refKey := ref.String()
	if c.terminal[refKey] && (!item.Checkpoint.Terminal || len(item.Events) != 0) {
		c.mu.Unlock()
		return false, fmt.Errorf("%w: consumption %s follows a terminal outcome", remote.ErrEventConflict, item.ID)
	}
	stored := cloneConsumption(item)
	c.seen[key] = append([]byte(nil), body...)
	c.entries = append(c.entries, stored)
	c.byRef[refKey] = append(c.byRef[refKey], stored)
	if item.Checkpoint.Terminal {
		c.terminal[refKey] = true
	}
	after := c.after
	c.mu.Unlock()
	if after != nil {
		after(cloneConsumption(item))
	}
	return true, nil
}

func (c *Consumer) Recover(ctx context.Context, ref remote.ProcessRef) ([]remote.Consumption, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recover != nil {
		return nil, c.recover
	}
	items := c.byRef[ref.String()]
	out := make([]remote.Consumption, 0, len(items))
	for _, item := range items {
		out = append(out, cloneConsumption(item))
	}
	return out, nil
}

func (c *Consumer) SetRecoverFault(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recover = err
}

func (c *Consumer) SetFault(fn func(remote.Consumption) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fault = fn
}

// SetAfterCommit runs fn after a new consumption is durable and after the
// consumer lock is released. Crash-window tests use it to stop the daemon at
// the one boundary a plain return-value fake cannot reach.
func (c *Consumer) SetAfterCommit(fn func(remote.Consumption)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.after = fn
}

func (c *Consumer) Entries() []remote.Consumption {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]remote.Consumption, 0, len(c.entries))
	for _, item := range c.entries {
		out = append(out, cloneConsumption(item))
	}
	return out
}

func cloneConsumption(in remote.Consumption) remote.Consumption {
	out := in
	out.Checkpoint.Tail = append([]byte(nil), in.Checkpoint.Tail...)
	out.Events = make([]core.Event, len(in.Events))
	for i, event := range in.Events {
		out.Events[i] = event
		if event.Usage != nil {
			usage := *event.Usage
			out.Events[i].Usage = &usage
		}
	}
	if in.Envelope != nil {
		env := *in.Envelope
		env.Payload = append([]byte(nil), in.Envelope.Payload...)
		out.Envelope = &env
	}
	if in.Gap != nil {
		gap := *in.Gap
		out.Gap = &gap
	}
	return out
}
