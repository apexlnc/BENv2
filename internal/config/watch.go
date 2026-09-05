package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is the settling window after a filesystem event (SPEC §5.4).
// One editor save can produce several events — a temp file created, written,
// then renamed over the target — and reloading on each would parse a file
// mid-write.
const DefaultDebounce = 200 * time.Millisecond

// WatchOptions are the knobs a caller may override; the zero value is the
// documented behavior apart from BuildRuntime, which is required.
type WatchOptions[R any] struct {
	// Debounce overrides DefaultDebounce.
	Debounce time.Duration
	// Logger receives the operator-visible reload errors (SPEC §5.4).
	Logger *slog.Logger

	// BuildRuntime constructs the adapter set for a definition and returns it,
	// so that definition and adapters can be published together (SPEC §5.4,
	// §5.7). It runs once at startup and again for every reload that moves an
	// adapter's configuration; `changed` says which adapters must be
	// reconstructed and re-checked Ready (BUILD.md assembly decision 13).
	//
	// It returns the runtime rather than installing one, because an adapter
	// checked Ready against one configuration and then operating under another
	// makes the check meaningless (§5.7). Publication is this package's, and it
	// happens only after this returns without error.
	//
	// prev is the runtime currently in force, so a dependency whose own
	// configuration did not move can be carried forward. It is a parameter and
	// not something to close over: a builder that captured the last runtime it
	// returned would be a second owner of the published state, which is the
	// ownership problem this seam exists to remove. At startup prev is the zero
	// R.
	//
	// An error refuses the reload exactly as a parse failure would: dispatch
	// blocks, last-known-good stands (§5.4). At startup it refuses to start,
	// since there is no last-known-good (§5.7). Either way the builder owns
	// disposal of whatever it constructed before failing — nothing half-built
	// may escape, and nothing returned alongside an error is published.
	//
	// The watcher does not construct adapters itself: the kind registry is not
	// its business (SPEC §5.7). It reports what changed and lets the caller
	// decide.
	BuildRuntime func(ctx context.Context, def *WorkflowDefinition, prev R, changed AdapterChange) (R, error)

	// Barrier, when non-nil, runs a commit at a point the caller has linearized
	// with its own identity-creating work, and reports whether it ran.
	//
	// It exists because some rebuilds may not be adopted while work is
	// outstanding — a claim belongs to the principal that made it, a worktree to
	// the root it was created under — and only the caller knows whether any is.
	// The caller cannot answer that from another goroutine either: between
	// reading "nothing outstanding" and this publication it can have claimed an
	// issue. So the test and the commit have to happen at one point the caller
	// controls, which is what this hands it.
	//
	// What it is handed is a single store and cannot fail. That is what makes
	// the exactly-once contract satisfiable: the caller MUST run it exactly once
	// or not at all, and MUST report which. A commit that ran while this
	// returned an error would publish a configuration the watcher has just
	// reported as a failed reload, leaving two answers to what is in force.
	// Notification and logging belong after the commit is established.
	//
	// An error wrapping ErrWorkOutstanding means "refused for now, ask again
	// when my work drains", and the watcher defers rather than rebuilding per
	// tick (see Quiescent). Any other error is an ordinary failed reload.
	//
	// prev and next are the runtimes it is publishing between, because *whether*
	// outstanding work is an obstacle depends on what moved: a policy edit, a hook
	// change, or a credential rotation that keeps the same principal has nothing
	// for a claim to be bound to, and refusing those while work exists would mean
	// no reload lands at all while the daemon is busy — §5.4 gives every one of
	// them to the reload. What counts as an identity is the caller's to decide;
	// this package must not know (SPEC §5.7).
	//
	// Not consulted at startup: there is no caller yet, and nothing outstanding.
	Barrier func(ctx context.Context, prev, next R, commit func()) error

	// Quiescent reports whether the caller currently has no outstanding work.
	// Advisory and cheap — it may be stale the moment it returns, and Barrier is
	// the authority.
	//
	// It exists only to keep a deferred candidate from being rebuilt on every
	// tick. Revalidate runs per dispatch cycle and a deferred candidate still
	// differs from what is in force, so without this the daemon would re-run New
	// and Ready — network I/O, against the §8.5 budget — once per tick for as
	// long as an operator's edit is refused. Nil means always quiescent, which
	// is the right default for a caller with no notion of outstanding work.
	Quiescent func() bool

	// newTimer supplies the settling window, and exists so a test can close
	// the window itself. Unexported because it is a seam, not a knob: the
	// debounce an operator cares about is a duration.
	newTimer func() debounceTimer
}

// debounceTimer is the settling window of SPEC §5.4, taken as an interface so
// a test can end the window on demand. Coalescing is a claim about what does
// *not* happen between the first event and the end of the window, and a test
// that sleeps past a real one asserts against the scheduler instead: on a
// loaded machine the first event's window can expire before the last write of
// a burst lands.
type debounceTimer interface {
	// Reset opens a window of d, replacing any window still open. Contract
	// is time.Timer's, including the Go 1.23 guarantee that no value from
	// the replaced window can still be received — which is why the watch
	// loop neither stops nor drains it.
	Reset(d time.Duration) bool
	// C delivers the end of the window.
	C() <-chan time.Time
}

// realTimer is the production debounceTimer.
type realTimer struct{ t *time.Timer }

// newRealTimer returns a stopped timer: the watcher is idle until an event
// arrives, and only then opens a window.
func newRealTimer() debounceTimer {
	t := time.NewTimer(time.Hour) // arbitrary; stopped before it can elapse
	t.Stop()
	return realTimer{t}
}

func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r realTimer) C() <-chan time.Time        { return r.t.C }

// AdapterChange reports which adapter configurations a reload moved. An
// adapter checked ready against one configuration and then operating under
// another would make the check meaningless (SPEC §5.7), so a true flag means
// that adapter must be reconstructed before it is used again.
//
// The three are not independent, and the builder owns the cascade: the
// workspace strategy is constructed from the repository the tracker names
// (§6.2, §10.2), and the publish-evidence checker from the workspace provider
// and the tracker together (§9.7). A rebuilt tracker therefore obliges a
// rebuilt workspace provider and checker, whatever these flags say about the
// file — carrying either forward would leave it bound to the previous
// repository or credential.
type AdapterChange struct {
	Tracker bool
	Agent   bool
	// Workspace covers workspace.* and hooks.*: the root the worktrees live
	// under, and the hook scripts the provider runs. Without it an edited
	// after_create publishes a new definition and rebuilds nothing, leaving the
	// provider running the previous scripts under the previous root while the
	// reload logs as adopted.
	Workspace bool
}

// Any reports whether any adapter needs reconstructing.
func (c AdapterChange) Any() bool { return c.Tracker || c.Agent || c.Workspace }

// Watcher keeps a RuntimeSource current against the WORKFLOW.md it watches (SPEC §5.4).
//
// What it hands out is immutable. A reload stores a new snapshot; it never
// mutates one a caller is holding, which is what makes "in-flight runs are never
// restarted by a reload" true by construction — work keeps the snapshot it
// started with.
type Watcher[R any] struct {
	path     string
	debounce time.Duration
	log      *slog.Logger

	build     func(context.Context, *WorkflowDefinition, R, AdapterChange) (R, error)
	barrier   func(context.Context, R, R, func()) error
	quiescent func() bool

	newTimer func() debounceTimer

	fsw  *fsnotify.Watcher
	done chan struct{}
	stop context.CancelFunc
	once sync.Once

	// resolved is path with its symlink chain followed, as of the last event.
	// Compared per event, because a change can reach the file without ever
	// naming it — see concerns.
	//
	// chain is the directories currently watched *beyond* filepath.Dir(path):
	// one per link in that chain, plus the final target's. Held so a later swap
	// can drop the ones that are no longer on the path.
	//
	// links is the symlinks themselves, so an event naming one is recognised as
	// a change to our configuration. Removing a link resolves to nothing, which
	// is deliberately not "moved", so this is the only clause that sees it.
	//
	// Both unguarded deliberately: the watch goroutine is the only thing that
	// reads or writes them, and Watch sets them before that goroutine exists.
	resolved string
	chain    []string
	links    []string

	// src is the published state, with its own lock: readers must not queue
	// behind a slow rebuild.
	src RuntimeSource[R]

	// applyMu serializes reload transactions. A transaction — read the file,
	// rebuild adapters, commit — is not atomic, and left unserialized a slow
	// rebuild of version A could commit after version B, installing a
	// definition older than the adapters already built for the newer one.
	applyMu sync.Mutex
	// deferred is the candidate a Barrier refused for outstanding work, held so
	// the next tick can re-assert the block without rebuilding. Guarded by
	// applyMu, the only thing that touches it.
	deferred *deferral

	reloadsMu sync.Mutex
	// reloads counts applied reload attempts; tests synchronize on it.
	reloads int
}

// deferral is a refusal worth remembering: this exact candidate, refused
// because the caller had work outstanding.
type deferral struct {
	def *WorkflowDefinition
	err error
}

// Watch loads path, builds the runtime for it, then keeps both current until
// ctx is cancelled or Close is called. An invalid config at startup is a
// refusal, not a blocked state, and so is a runtime that cannot be built and
// made ready: there is no last-known-good to fall back to (SPEC §5.7).
//
// BuildRuntime is required. A reload that moves an adapter's configuration has
// to reconstruct it and re-check Ready before it is used (BUILD.md assembly
// decision 13), and the watcher cannot do that itself. Defaulting a missing
// builder to "adopt anyway" would satisfy that rule silently and wrongly.
func Watch[R any](ctx context.Context, path string, opts WatchOptions[R]) (*Watcher[R], error) {
	if opts.BuildRuntime == nil {
		return nil, ErrNoRuntimeBuilder
	}
	// Not contained: at startup a template-engine panic is the loud
	// incompatibility internal/template means it to be, with no in-flight
	// work to protect and no last-known-good to fall back to.
	def, err := Load(path)
	if err != nil {
		return nil, err
	}
	// Everything is new, so everything is built. The startup build goes through
	// the same seam as every reload's, which is what makes "Ready has passed
	// before publication" one rule with one implementation rather than two.
	var prev R
	runtime, err := opts.BuildRuntime(ctx, def, prev, AdapterChange{Tracker: true, Agent: true, Workspace: true})
	if err != nil {
		return nil, err
	}

	w := &Watcher[R]{
		path:      def.Path,
		debounce:  cmp.Or(opts.Debounce, DefaultDebounce),
		log:       cmp.Or(opts.Logger, slog.Default()),
		build:     opts.BuildRuntime,
		barrier:   opts.Barrier,
		quiescent: opts.Quiescent,
		newTimer:  opts.newTimer,
		done:      make(chan struct{}),
	}
	if w.newTimer == nil {
		w.newTimer = newRealTimer
	}
	// Revision 1, stored before the watch goroutine exists: no reader can
	// observe the zero value, and nothing is announced for it — the caller holds
	// this state as Watch's own result.
	w.src.store(def, runtime, nil)

	// Watch the parent directory, not the file. An editor's atomic save
	// replaces the inode, and a single-file watch follows the old one into
	// oblivion — silently, which is the dangerous part (SPEC §5.4).
	w.fsw, err = fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.fsw.Add(filepath.Dir(w.path)); err != nil {
		w.fsw.Close()
		return nil, err
	}
	// The starting point for concerns' resolved-path clauses. An unreadable path
	// leaves it empty, which disables them and leaves the name match — the
	// pre-existing behaviour — rather than failing startup over it: Load has
	// already read this file, so anything unreadable here is a race, not a
	// misconfiguration.
	//
	// The chain's directories are watched as well as our own, because a link can
	// be repointed anywhere: `WORKFLOW.md -> /srv/releases/current/WORKFLOW.md`
	// moves when somebody swaps `/srv/releases/current`, and no event for that
	// lands in our directory. Watching only ours would resolve correctly for a
	// projected ConfigMap — where `..data` is our sibling — and report nothing at
	// all for a release layout or a dotfiles checkout.
	w.resolved, _ = filepath.EvalSymlinks(w.path)
	w.syncChain()
	// Said once, at startup, because the alternative way to discover it is a
	// reload that never arrives.
	//
	// Keyed on the path itself being a link, not on `resolved != path`: on macOS
	// a temp directory sits under /var, which is a symlink to /private/var, so
	// the latter is true of entirely ordinary files and the notice would be noise
	// on every run.
	if fi, err := os.Lstat(w.path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		w.log.Info("workflow path is a symlink; a reload is detected by its resolved target changing",
			"path", w.path, "resolved", w.resolved, "watching", w.chain)
	}

	ctx, w.stop = context.WithCancel(ctx)
	go w.run(ctx)
	return w, nil
}

// RuntimeSource is the cell the configuration is published to. A caller that reads the
// configuration on a hot path holds this rather than the watcher.
func (w *Watcher[R]) RuntimeSource() *RuntimeSource[R] { return &w.src }

// Snapshot returns the configuration in force.
//
// There is deliberately no accessor for a single field. A definition and the
// runtime built from it are only meaningful together, and a caller able to read
// them apart is a caller able to pair a definition with adapters from another
// revision — which is the whole of what this package is for.
func (w *Watcher[R]) Snapshot() Snapshot[R] {
	snap, _ := w.src.Load()
	return snap
}

// Revalidate re-reads the file synchronously, applies the result, and returns
// what is then in force. It is the defensive pre-dispatch backstop of SPEC §5.4:
// watches drop events — a replaced directory, an editor writing through a path
// we are not watching, a platform quirk — and a queue silently frozen on stale
// config is worse than the cost of a parse per tick.
//
// It returns the snapshot rather than an error so a caller can use it as the
// preflight check directly: Blocked is the verdict, and it arrives paired with
// the definition and runtime it was reached under.
func (w *Watcher[R]) Revalidate(ctx context.Context) Snapshot[R] {
	w.apply(ctx)
	return w.Snapshot()
}

// Close stops watching. It is idempotent and safe to call after ctx
// cancellation.
func (w *Watcher[R]) Close() error {
	w.once.Do(func() {
		w.stop()
		<-w.done
	})
	return nil
}

func (w *Watcher[R]) run(ctx context.Context) {
	defer close(w.done)
	defer w.fsw.Close()

	// Stopped until an event arrives; every further event pushes it out.
	timer := w.newTimer()

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !w.concerns(event) {
				continue
			}
			// The chain may have moved, and the next swap will happen somewhere
			// the old watch set does not cover. Before the debounce, not after:
			// a second swap during the settling window must still be seen.
			w.syncChain()
			timer.Reset(w.debounce)

		case <-timer.C():
			w.apply(ctx)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// A watch-layer error is not a config verdict: the last-known-good
			// stands and Revalidate remains the backstop.
			w.log.Error("workflow watch error", "path", w.path, "error", err)
		}
	}
}

// concerns reports whether an event is about our file. The watch is on the
// directory, so most events are somebody else's — including the temp file an
// atomic save writes before renaming it over ours.
//
// Two clauses, because a change can reach the file without ever naming it.
//
// The name match is the ordinary case: an atomic save or an in-place write
// lands as an event *on* our name, and the debounce coalesces it with whatever
// preceded it.
//
// The resolved-path match is for a path that is a **symlink**. Kubernetes
// projects a ConfigMap as `WORKFLOW.md -> ..data/WORKFLOW.md` and updates it by
// writing a new timestamped directory and renaming a symlink onto `..data`. The
// entry named WORKFLOW.md is never touched, so on Linux no event carries its
// name and the name match alone reports nothing while the file's contents
// change underneath — silently, which is the dangerous part (SPEC §5.4).
// Measured: five events per update, none of them ours. macOS does not reproduce
// it, because kqueue reports the watched name as removed and recreated when the
// link resolves elsewhere, so the gap is invisible on a dev machine and live in
// the deployment (#158).
//
// Evaluated on **every** event, not a filtered subset. Which event in the batch
// first observes the new target is a race with the writer — the swap can land
// before the first event is delivered — and only the guarantee that some event
// follows the rename makes the detection sound.
//
// The cost is one EvalSymlinks per event, not a re-read: an unrelated sibling
// in the directory resolves the same way it did before and is still ignored.
// For a path that is not a symlink the resolution is itself, so this clause
// never fires and nothing about the ordinary case changes.
func (w *Watcher[R]) concerns(event fsnotify.Event) bool {
	// Resolved first, and unconditionally, so the baseline is refreshed on every
	// event including a name match. Returning early on the name match left it
	// stale: retargeting the link directly is seen by name, and a later change
	// that lands back on the *old* resolved path then compares equal to a value
	// that has not been true since — so the rollback is ignored and the daemon
	// serves a definition the file no longer holds.
	//
	// An error or the empty result means "no information": the last known
	// resolution stands, so a transient failure cannot make the next real change
	// look like a no-op.
	name := filepath.Clean(event.Name)
	cur, err := filepath.EvalSymlinks(w.path)
	moved := err == nil && cur != "" && cur != w.resolved
	target := w.resolved
	if moved {
		// Stored, not just compared: without this only the first swap is seen,
		// and a projection is updated as often as its ConfigMap is.
		w.resolved = cur
	}
	// Four ways a change reaches us:
	//   - the event names our path: an atomic save, an in-place write, or the
	//     link itself being repointed;
	//   - the resolution moved: something the path points *through* was swapped;
	//   - the event names the target we currently resolve to: the real file was
	//     edited in place under a link, which moves no path at all;
	//   - the event names a link on our chain. Removing one resolves to nothing,
	//     so `moved` is false by design — the baseline must survive a transient
	//     failure — and the event names neither us nor our target. Without this
	//     clause a deleted intermediate link is silently ignored and the daemon
	//     serves a configuration whose file no longer exists, where §5.4 requires
	//     the reload to fail and block dispatch.
	return name == w.path || moved ||
		(target != "" && name == target) ||
		slices.Contains(w.links, name)
}

// maxChainDepth bounds the symlink walk. A loop is not a configuration error
// worth failing startup over — EvalSymlinks reports it and the resolved-path
// clauses simply stay inert — but the walk that collects directories to watch
// has no such backstop of its own.
const maxChainDepth = 32

// walkChain reports what must be watched for a change to reach path.
//
// dirs are the directories: path's own, one for each link along the way, and the
// final target's. links are the symlinks themselves, because removing one is a
// change to our configuration whose event names neither path nor its target.
// resolved says the walk reached a real file — false means a broken link or a
// loop, which is no information rather than an empty chain.
//
// Deliberately not EvalSymlinks, which answers only where the path *ends*. The
// directories in between are where the swap happens, and an intermediate link is
// exactly what a release layout or a projected ConfigMap moves.
func walkChain(path string) (dirs, links []string, resolved bool) {
	root := string(filepath.Separator)
	seen := map[string]bool{}
	add := func(d string) {
		// Never the filesystem root. On macOS /var is a symlink to /private/var,
		// so every path under a temp directory walks through it and the root
		// would join the watch set of every daemon on the platform — to detect
		// somebody repointing /var, which is not a thing that happens.
		if d == "" || d == root {
			return
		}
		// Deduplicated by **spelling**, not by where the directory resolves to.
		//
		// Resolving looks tidier — two spellings of one directory are one inode,
		// and watching it twice doubles its events — but it is wrong, and the
		// reason is that fsnotify pins a watch to whatever a path resolved to when
		// it was added. A spelling that runs through a link is therefore a watch on
		// *the generation that link pointed at*, and treating it as covering the
		// physical directory leaves the physical directory unwatched once the link
		// moves. `current/config/WORKFLOW.md` is the shape: the config's own
		// directory is not itself a link, so nothing about it looks special, and
		// yet the watch on it is pinned to `v1/config` for the life of the process.
		//
		// So the physical target is always its own entry. What that costs is one
		// redundant watch wherever a spelling and its resolution differ — every
		// path under a temp directory on macOS, since /var is a symlink to
		// /private/var — and what it buys is that the directory the file actually
		// lives in is watched by its physical name on every platform. Doubled
		// events coalesce in the debounce; a missing watch does not come back.
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}

	cur := filepath.Clean(path)
	// Where an atomic save lands, and the one spelling whose events carry a name
	// concerns can match directly.
	own := filepath.Dir(cur)
	add(own)

	// A link can sit on any component, not only the last one. `releases/current`
	// is a directory link, and it is the component a release layout swaps —
	// following only the leaf would resolve nothing and watch nowhere useful.
	for range maxChainDepth {
		link, rest, ok := firstSymlink(cur)
		if !ok {
			break
		}
		// The directory holding the link is what must be watched: the swap
		// replaces an entry in it, and that is the event. The link itself is
		// recorded too — removing it is a change to our configuration, and the
		// event names the link rather than us or our target.
		add(filepath.Dir(link))
		links = append(links, link)
		target, err := os.Readlink(link)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		cur = filepath.Clean(filepath.Join(target, rest))
	}
	// Where the file actually lives, so an in-place edit under the links is seen.
	// After a loop or a broken link this is wherever the walk gave up, which
	// watches something harmless rather than nothing.
	add(filepath.Dir(cur))

	// Resolved is decided by whether the endpoint exists, not by the walk running
	// out of links: firstSymlink reports "no link here" and "this component is
	// missing" the same way, and a removed intermediate link is the second. Stat
	// rather than Lstat, so a dangling final link counts as unresolved too, and a
	// loop fails with ELOOP.
	_, err := os.Stat(cur)
	return dirs, links, err == nil
}

// firstSymlink returns the shortest prefix of path that is itself a symlink,
// together with the components after it. ok is false when no component is a
// link, which is the terminating case for the walk above.
func firstSymlink(path string) (link, rest string, ok bool) {
	sep := string(filepath.Separator)
	parts := strings.Split(filepath.Clean(path), sep)
	prefix := ""
	for i, p := range parts {
		if i == 0 && p == "" {
			prefix = sep // absolute path: the leading empty component is the root
			continue
		}
		prefix = filepath.Join(prefix, p)
		fi, err := os.Lstat(prefix)
		if err != nil {
			return "", "", false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return prefix, filepath.Join(parts[i+1:]...), true
		}
	}
	return "", "", false
}

// syncChain points the watch set at the current chain: adds directories that
// have joined it, drops those that have left. Our own directory is never
// dropped — it is where an atomic save lands, whatever the links do.
//
// Called at startup and after every concerning event, because a swap moves the
// chain and the next swap will happen somewhere the old set does not cover.
// It deliberately does not touch resolved. That baseline belongs to concerns,
// which only advances it on a successful resolution; refreshing it here would
// clobber the last known value with the empty result of a transient failure —
// the very thing concerns is careful not to do.
func (w *Watcher[R]) syncChain() {
	own := filepath.Dir(w.path)
	dirs, links, resolved := walkChain(w.path)

	want := map[string]bool{}
	var next []string
	for _, d := range dirs {
		if d == own {
			// The base watch is Watch's, added before this ever runs, and it is
			// what makes an event carry a name concerns can match directly.
			continue
		}
		want[d] = true
		if err := w.fsw.Add(d); err != nil {
			// Not fatal: the directory may have been removed between the walk and
			// the add, and the resolved-path clauses still cover anything that
			// lands in a directory we do hold.
			w.log.Debug("could not watch a directory on the workflow's symlink chain",
				"dir", d, "error", err)
			continue
		}
		next = append(next, d)
	}

	if !resolved {
		// A walk that did not reach a real file has not told us the chain is
		// smaller — only that it cannot see all of it right now. Pruning on that
		// would drop the watch on the directory holding the *missing* link, and
		// restoring it would then be invisible: the block raised by the broken
		// configuration could never clear without a restart. Keep everything and
		// let the next successful walk do the pruning.
		for _, d := range w.chain {
			if !want[d] {
				next = append(next, d)
			}
		}
		w.chain = next
		w.links = union(w.links, links)
		return
	}

	for _, d := range w.chain {
		if !want[d] {
			_ = w.fsw.Remove(d)
		}
	}
	w.chain = next
	w.links = links
}

// union appends the entries of b that a does not already hold, order preserved.
func union(a, b []string) []string {
	for _, v := range b {
		if !slices.Contains(a, v) {
			a = append(a, v)
		}
	}
	return a
}

// outcome is what one reload transaction concluded: a candidate to adopt, a
// failure to publish as a block, or neither.
type outcome[R any] struct {
	def     *WorkflowDefinition
	runtime R
	err     error
}

// apply runs one reload transaction and installs the result: a clean reload
// becomes current and clears any block, a failed one leaves the definition and
// runtime untouched and blocks new dispatches (SPEC §5.4).
//
// One at a time. The watch goroutine and a caller's Revalidate can both land
// here.
func (w *Watcher[R]) apply(ctx context.Context) {
	w.applyMu.Lock()
	defer w.applyMu.Unlock()

	cur, _ := w.src.Load()
	out := w.transaction(ctx, cur)

	if out.err == nil && out.def == nil {
		// Nothing moved. Lift any standing block: the failure was transient,
		// a half-written file that has settled back to what we already had.
		w.clearBlock(cur)
		return
	}

	if out.err == nil {
		out.err = w.commit(ctx, cur, out.def, out.runtime)
		if out.err == nil {
			w.deferred = nil
			w.countReload()
			if cur.Blocked != nil {
				w.log.Info("workflow reload succeeded; dispatch unblocked", "path", w.path)
			}
			return
		}
		if errors.Is(out.err, ErrWorkOutstanding) {
			// Refused for now, not faulty. Remembering the candidate is what
			// keeps the next tick from paying for New and Ready again to be told
			// the same thing (see WatchOptions.Quiescent).
			w.deferred = &deferral{def: out.def, err: out.err}
		}
	}

	// A failure of any kind publishes the block against the standing definition
	// and runtime. The block is *published*, not merely returned: leaving the
	// snapshot untouched would leave dispatch permitted while the file on disk
	// is one this daemon has refused (SPEC §5.4).
	w.src.store(cur.Definition, cur.Runtime, out.err)
	w.countReload()
	if cur.Blocked == nil || cur.Blocked.Error() != out.err.Error() {
		// Log the transition, not the state. Revalidate runs every tick, so
		// logging the standing error each time would bury the event that
		// matters; the durable surface for "still broken" is the blocked state
		// itself, which `ben status` renders.
		w.log.Error("workflow reload failed; new dispatches are blocked until it is fixed",
			"path", w.path, "error", out.err)
	}
}

// commit publishes an adopted candidate — through the caller's barrier when it
// has one, so the publication is linearized with the caller's own
// identity-creating work.
//
// What the barrier is handed is a single store and nothing else. That is what
// makes its exactly-once contract satisfiable: there is no sequence of installs
// for a failure to land in the middle of, so "ran" or "did not run" is the whole
// truth about the published state.
func (w *Watcher[R]) commit(ctx context.Context, cur Snapshot[R], def *WorkflowDefinition, runtime R) error {
	publish := func() { w.src.store(def, runtime, nil) }
	if w.barrier == nil {
		publish()
		return nil
	}
	return w.barrier(ctx, cur.Runtime, runtime, publish)
}

// transaction is the fallible half of a reload: read the file, decide what
// moved, rebuild the adapters that moved. It publishes nothing. A nil def with
// a nil error means nothing changed, so the caller can tell "no-op" from
// "adopt this".
//
// Panics are contained here, and the whole transaction is inside the net, not
// just the parse. internal/template's load-time filter probe deliberately
// lets engine panics propagate — at startup that is right, a loud
// incompatibility with nothing in flight and no last-known-good to fall back
// to — and an adapter constructor can panic just as easily. During reload
// neither may kill the daemon: §5.4 says never crash and never disturb
// in-flight runs, so a panic becomes an ordinary failed reload.
func (w *Watcher[R]) transaction(ctx context.Context, cur Snapshot[R]) (out outcome[R]) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		out = outcome[R]{err: fmt.Errorf("%w: %v", ErrReloadPanic, r)}
		// The stack is the only diagnosis a panic leaves, and a panic is rare
		// enough to log every time rather than per transition.
		w.log.Error("workflow reload panicked; keeping the last-known-good config",
			"path", w.path, "panic", r, "stack", string(debug.Stack()))
	}()

	def, err := Load(w.path)
	if err != nil {
		return outcome[R]{err: err}
	}
	changed := adapterChange(cur.Definition, def)
	if !changed.Any() && sameDefinition(cur.Definition, def) {
		return outcome[R]{}
	}
	if d := w.deferred; d != nil && sameDefinition(d.def, def) && !w.isQuiescent() {
		// The caller refused this exact candidate and its work has not drained.
		// Re-assert the standing block without rebuilding: New and Ready cannot
		// answer differently, and asking them per tick spends requests to hear
		// it (§8.5).
		return outcome[R]{err: d.err}
	}
	// SPEC §5.2.9: the deployment declaration is process-lifetime. A reload that
	// changes it is invalid, which lands it on §5.4's existing path — keep the
	// last-known-good declaration for in-flight runs, block new dispatch, and
	// require a restart to adopt it.
	//
	// Refused here rather than adopted quietly because the declaration is not a
	// fact about the *workflow*, which a reload may legitimately change; it is
	// an assertion about how this process was launched and what the operator
	// arranged around it. A running daemon cannot have been re-launched, and
	// `attended` in particular asserts something about a human that editing a
	// file does not make true.
	if cur.Definition != nil && cur.Definition.Config.Deployment != def.Config.Deployment {
		return outcome[R]{err: fmt.Errorf("%w: deployment declaration changed (%s → %s); "+
			"it is process-lifetime configuration and a restart adopts it (SPEC §5.2.9, §10.1)",
			ErrDeploymentChanged, cur.Definition.Config.Deployment.Mode, def.Config.Deployment.Mode)}
	}
	// The substrate declaration is process-lifetime for a related but distinct
	// reason (#194). Deployment is an assertion a reload cannot make true; this
	// is an identity outstanding *work* is bound to. A daemon holding remote
	// claims has sandboxes, run bindings and event cursors addressed against one
	// backend, and a reload that moved the substrate under them would leave every
	// one of those unattachable — while a live agent kept running somewhere BEN
	// had stopped looking. Same landing as above: last-known-good stands,
	// dispatch blocks, a restart adopts it.
	if cur.Definition != nil && cur.Definition.Config.SubstrateBinding() != def.Config.SubstrateBinding() {
		return outcome[R]{err: fmt.Errorf("%w: substrate declaration changed (%s → %s); "+
			"it is process-lifetime configuration and a restart adopts it, because outstanding claims "+
			"address the backend they were dispatched to (#194, #46)",
			ErrSubstrateChanged, cur.Definition.Config.Substrate.Kind, def.Config.Substrate.Kind)}
	}
	// The review controller is process-lifetime for both of those reasons at
	// once (#204). Its outstanding work is addressed against a backend, like the
	// substrate's; and its three identities are what make every author check on
	// the forge mean anything, so moving one under an in-flight round could
	// route on artifacts a different login wrote. Same landing: last-known-good
	// stands, dispatch blocks, a restart adopts it.
	if cur.Definition != nil && !sameReviewBinding(cur.Definition.Config.ReviewBinding(), def.Config.ReviewBinding()) {
		return outcome[R]{err: fmt.Errorf("%w: review declaration changed (enabled %t → %t); "+
			"it is process-lifetime configuration and a restart adopts it, because outstanding review runs "+
			"address the reviewer they were dispatched to and its identities decide what the forge is read as (#204, #11)",
			ErrReviewChanged, cur.Definition.Config.Review.Enabled, def.Config.Review.Enabled)}
	}

	runtime := cur.Runtime
	if changed.Any() {
		// An adapter that cannot be rebuilt and made ready under the new
		// configuration is a failed reload, not a warning.
		runtime, err = w.build(ctx, def, cur.Runtime, changed)
		if err != nil {
			return outcome[R]{err: err}
		}
	}
	return outcome[R]{def: def, runtime: runtime}
}

// isQuiescent reports the caller's advisory verdict. A caller with no notion of
// outstanding work never defers, so a missing hook means quiescent.
func (w *Watcher[R]) isQuiescent() bool {
	return w.quiescent == nil || w.quiescent()
}

// clearBlock lifts a standing block when the file reloads clean but
// unchanged — the case where a failure was transient (a half-written file
// that has since settled back to what we already had).
func (w *Watcher[R]) clearBlock(cur Snapshot[R]) {
	if cur.Blocked == nil {
		// Nothing moved and nothing was standing. Revalidate lands here on every
		// tick of a healthy daemon, so storing would be a write per tick that
		// says only what is already in force.
		return
	}
	w.deferred = nil
	w.src.store(cur.Definition, cur.Runtime, nil)
	w.log.Info("workflow reload succeeded; dispatch unblocked", "path", w.path)
}

// sameDefinition reports whether a reload produced the definition already in
// force.
//
// Provenance is part of it, not a derivative: replacing a literal secret with
// a $VAR that resolves to the same string leaves the config identical and
// changes only where the value came from — and provenance is what drives
// redaction in `config effective` (SPEC §5.8). Skipping that reload would
// keep printing a secret the file no longer spells out.
func sameDefinition(current, next *WorkflowDefinition) bool {
	return current != nil &&
		current.PromptTemplate == next.PromptTemplate &&
		reflect.DeepEqual(current.Config, next.Config) &&
		reflect.DeepEqual(current.Provenance, next.Provenance)
}

// adapterChange compares the slice of configuration each adapter is bound to.
// It is the whole adapter config, not just the opaque block: a rule like
// non-empty required_labels spans both, and checking a new provider block
// against a previous reload's core fields would be a silent hot-reload bug
// (SPEC §5.7).
//
// Both legs compare a **name-free binding** rather than listing the sections
// that make one up, because listing them is how this went wrong three times:
// hooks and workspace.root were missing until they were noticed, `publish` was
// missing when the section was introduced (#117), and a `credential_sources`
// edit beneath an unchanged name would have been the third. The value an adapter
// is bound to and the reason to rebuild it are now the same expression, so a new
// core-owned field cannot reach New without also being a reason to re-check
// Ready.
//
// The tracker leg is the binding and **not** raw Config.Tracker, which is what a
// credential source obliges: the provider block carries `credential_source:
// <name>`, so a rename would rebuild however name-free the descriptor is. The
// binding excludes the name and carries the source's canonical BindingKey
// instead — the complete definition — so an edit beneath an unchanged name
// rebuilds and a rename with an identical definition does not. It also excludes
// `token`, which the key's SHA-256 digest of a loader-resolved literal replaces:
// comparing the resolved secret would work, and would carry it across a boundary
// whose purpose is to be non-secret.
//
// The workflow key stays out of both, for the reason it always has: it derives
// from the watched path and cannot move under a running daemon.
func adapterChange(current, next *WorkflowDefinition) AdapterChange {
	if current == nil {
		return AdapterChange{Tracker: true, Agent: true, Workspace: true}
	}
	return AdapterChange{
		// Kind too on both legs, which neither binding carries: the registry
		// resolves it *to* the adapter, so it selects which one is built rather
		// than what it is built from, and a binding carrying it would conflate
		// the two.
		Tracker: current.Config.Tracker.Kind != next.Config.Tracker.Kind ||
			!reflect.DeepEqual(current.Config.TrackerBinding(), next.Config.TrackerBinding()),
		Agent: current.Config.Agent.Kind != next.Config.Agent.Kind ||
			!reflect.DeepEqual(current.Config.AgentBinding(), next.Config.AgentBinding()),
		// The provider binds the root it creates worktrees under and the hook
		// scripts it runs, so both belong to it.
		Workspace: !reflect.DeepEqual(current.Config.Workspace, next.Config.Workspace) ||
			!reflect.DeepEqual(current.Config.Hooks, next.Config.Hooks),
	}
}

func (w *Watcher[R]) countReload() {
	w.reloadsMu.Lock()
	w.reloads++
	w.reloadsMu.Unlock()
}

// reloadCount is the number of apply attempts so far; tests use it to wait
// for the debounce rather than sleeping a guessed interval.
func (w *Watcher[R]) reloadCount() int {
	w.reloadsMu.Lock()
	defer w.reloadsMu.Unlock()
	return w.reloads
}
