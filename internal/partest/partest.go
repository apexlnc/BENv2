// Package partest bounds how wide a cohort of parallel tests may run.
//
// `make check` runs `go test -race -cover ./...`, and its wall clock is the
// slowest package rather than the sum: Go already runs packages concurrently.
// So the only way to shorten it is to shorten the critical-path package, and
// inside a package the lever is `t.Parallel`.
//
// Unbounded is the wrong amount of it. The suites this exists for
// (internal/agent/agenttest, internal/workspace) drive real child processes —
// a re-exec of the race-and-coverage-instrumented test binary, or git — so
// `t.Parallel` alone would fan out to `-parallel`, which defaults to
// GOMAXPROCS and is a property of whoever's machine is running. Trading elapsed
// time for load is exactly how a suite built to expose load-sensitive
// synchronization stops being able to. A cohort therefore states its own width,
// independent of the host, and that width is asserted rather than assumed.
//
// A Gate is not a substitute for the classification that precedes it: what may
// join a cohort at all is each suite's judgement, made case by case and
// recorded there. This package answers only "how many at once".
package partest

import (
	"flag"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Gate admits at most a fixed number of a cohort's tests at a time.
//
// The zero Gate is unusable; construct one with New.
type Gate struct {
	bound  int
	tokens chan struct{}

	mu      sync.Mutex
	held    int
	peak    int
	entered int
	// names are the members that have taken a slot, kept so Enter can refuse a
	// nested one before it deadlocks rather than after.
	names []string
	// groups are the parents whose subtests are the members. They hold no slot,
	// so a member inside one is the ordinary case rather than the refused one.
	groups []string
}

// New returns a gate admitting bound tests at once.
func New(bound int) *Gate {
	if bound < 1 {
		panic(fmt.Sprintf("partest.New(%d): a cohort admitting nobody never finishes", bound))
	}
	return &Gate{bound: bound, tokens: make(chan struct{}, bound)}
}

// Enter marks t as parallel and blocks until the cohort has room for it. The
// slot is returned when t and its subtests finish.
//
// Deliberately one call rather than a `t.Parallel()` beside a gate acquisition:
// the two must not drift apart, and a test that took a slot without pausing —
// or paused without taking one — would be counted by exactly one of the two
// mechanisms that are supposed to agree.
//
// Blocking here parks a test the `testing` package already counts as running,
// so `-parallel` must be at least the bound for the cohort to reach its width.
// It is never a deadlock: a slot is held only for the duration of a test that
// is free to finish.
func (g *Gate) Enter(t *testing.T) {
	t.Helper()
	if ancestor := g.enclosing(t.Name()); ancestor != "" {
		// A slot is held until its test's cleanups run, and a parent's cleanups
		// run after its subtests. So a parent inside the cohort holding a slot
		// while its children queue for one deadlocks the moment `bound` such
		// parents are in flight — with no output, because every goroutine is
		// blocked on a channel nobody will send to. A panic naming both tests is
		// the cheaper failure.
		panic(fmt.Sprintf("partest: %s entered the cohort inside %s, which is already in it: "+
			"a test and its subtests may not both take a slot, or the cohort deadlocks at %d "+
			"nested parents", t.Name(), ancestor, g.bound))
	}
	t.Parallel()
	g.acquire(t.Name())
	t.Cleanup(g.release)
}

// EnterGroup marks t as parallel without taking a slot, for a test whose
// *subtests* are the cohort members.
//
// A table-driven test is often the longest member a cohort has, and splitting
// it is the difference between the cohort's width applying to it and the whole
// table running end to end inside one slot. The parent cannot simply Enter as
// well — see Enter — so it takes no slot at all: it does no work of its own
// beyond starting its subtests, and every one of those is gated.
//
// This costs nothing in width. The `testing` package releases a parent's
// `-parallel` slot before it waits for its subtests, so a group holds neither
// of the two resources while its children run.
func (g *Gate) EnterGroup(t *testing.T) {
	t.Helper()
	if ancestor := g.enclosing(t.Name()); ancestor != "" {
		panic(fmt.Sprintf("partest: %s opened a group inside %s, which holds a slot: "+
			"its subtests would queue behind their own parent", t.Name(), ancestor))
	}
	g.mu.Lock()
	g.groups = append(g.groups, t.Name())
	g.mu.Unlock()
	t.Parallel()
}

// enclosing returns an ancestor of name that holds a slot, or "". A group is
// not one: it holds nothing, which is the point of it.
//
// Sound without further synchronization: a subtest exists only because its
// parent's body is running, and Enter is the first thing a member does.
func (g *Gate) enclosing(name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, in := range g.names {
		if strings.HasPrefix(name, in+"/") {
			return in
		}
	}
	return ""
}

func (g *Gate) acquire(name string) {
	g.tokens <- struct{}{}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held++
	g.entered++
	if name != "" {
		g.names = append(g.names, name)
	}
	if g.held > g.peak {
		g.peak = g.held
	}
}

func (g *Gate) release() {
	g.mu.Lock()
	g.held--
	g.mu.Unlock()
	<-g.tokens
}

// Peak is the most tests this cohort ever held at once.
func (g *Gate) Peak() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// Entered is how many tests have taken a slot.
func (g *Gate) Entered() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entered
}

// emptyGroups are the groups no member ever ran inside.
//
// A group takes no slot, which is safe only because its subtests take one each.
// One whose subtests never joined is therefore a test running parallel and
// entirely ungated — the single way through this package to exceed the bound
// without the peak ever recording it, since what escaped was never counted.
func (g *Gate) emptyGroups() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, group := range g.groups {
		inhabited := false
		for _, member := range g.names {
			if strings.HasPrefix(member, group+"/") {
				inhabited = true
				break
			}
		}
		if !inhabited {
			out = append(out, group)
		}
	}
	return out
}

// Check reports whether the cohort actually ran the way it says it does.
//
// Register it with t.Cleanup on the test that owns the cohort, not after the
// loop that starts it: `t.Run` returns as soon as a parallel subtest pauses, so
// the peak is not final until every one of them has finished — which is
// precisely when cleanups registered on their parent run.
//
// Two directions, because a cohort has two ways to be wrong and only one of
// them is loud. Running wider than the bound is the load the bound exists to
// prevent. Running narrower is the silent one: a cohort that has quietly
// serialized still passes every test it contains, and the only symptom is
// `make check` taking longer, which is not a failure anyone is shown.
func (g *Gate) Check(t *testing.T) {
	t.Helper()
	for _, problem := range g.Problems() {
		t.Error(problem)
	}
}

// Problems is Check's verdict as data, for a cohort whose members are top-level
// tests: there is no parent to hang a cleanup on, so TestMain reads this after
// m.Run and fails the binary itself.
func (g *Gate) Problems() []string { return g.problems(parallelLimit()) }

// problems takes the `-parallel` ceiling as an argument so this package's own
// tests can drive it over a gate that is deliberately misbuilt — which a gate
// reached through New can never be.
//
// limit is 0 when the flag cannot be read.
func (g *Gate) problems(limit int) []string {
	peak, entered := g.Peak(), g.Entered()
	var out []string
	for _, group := range g.emptyGroups() {
		out = append(out, fmt.Sprintf(
			"%s opened a cohort group but none of its subtests joined: it ran parallel and "+
				"ungated, which the peak above cannot see because nothing counted it", group))
	}
	if peak > g.bound {
		out = append(out, fmt.Sprintf(
			"cohort ran %d tests at once against a bound of %d: the gate is not holding, and "+
				"the child-process fan-out this bound exists to cap is unbounded", peak, g.bound))
	}
	// The width check needs a cohort big enough for its width to mean
	// something. `go test -run` selecting three of a hundred cases is not a
	// regression, and neither is three short cases failing to overlap; twice the
	// bound is the smallest cohort that cannot fail to fill itself by accident.
	if entered < 2*g.bound {
		return out
	}
	want := g.bound
	if limit > 0 && limit < want {
		want = limit
	}
	if peak < want {
		out = append(out, fmt.Sprintf(
			"cohort peaked at %d, want %d (bound %d, -parallel %d, %d tests entered): it has "+
				"serialized, which no test in it would notice — only a slower `make check`",
			peak, want, g.bound, limit, entered))
	}
	return out
}

// parallelLimit reports the `-parallel` value in force, or 0 if it cannot be
// read. Read from the flag rather than from GOMAXPROCS: GOMAXPROCS is only its
// default, and `go test -parallel=1` is a real way to run a suite.
func parallelLimit() int {
	f := flag.Lookup("test.parallel")
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	n, ok := g.Get().(int)
	if !ok {
		return 0
	}
	return n
}
