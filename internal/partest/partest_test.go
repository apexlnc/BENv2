package partest

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A cohort driven through the real Enter path never exceeds its bound, and does
// reach it.
//
// Both halves through `t.Run` and `t.Parallel` rather than through acquire
// directly: the pairing of "pause" and "take a slot" is what Enter exists to
// keep together, and a hand-rolled driver would exercise the accounting while
// skipping the part that has to agree with the `testing` package.
func TestGateHoldsItsBoundThroughEnter(t *testing.T) {
	const (
		bound = 2
		cases = 8
	)
	g := New(bound)
	// Registered before the subtests are started, and therefore run after every
	// one of them has finished — see Gate.Check.
	t.Cleanup(func() { g.Check(t) })

	for i := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			g.Enter(t)
			// Long enough that a gate admitting too many would actually hold them
			// at once, rather than finishing one before the next arrives.
			time.Sleep(20 * time.Millisecond)
		})
	}
}

// The check fails when the cohort runs wider than its bound.
//
// This is the assertion the acceptance criterion is about, and it cannot be
// made against a gate from New — New builds a channel exactly as wide as the
// bound, so the two agree by construction. The regression it stands in for is
// somebody widening one and not the other, which is reproduced here directly.
func TestOverrunIsReported(t *testing.T) {
	const bound = 3
	// One wider than it claims to be.
	g := &Gate{bound: bound, tokens: make(chan struct{}, bound+1)}
	holdAtOnce(t, g, bound+1)

	if got := g.Peak(); got != bound+1 {
		t.Fatalf("peak = %d, want %d: the overrun never happened, so its detection proves nothing", got, bound+1)
	}
	problems := g.problems(bound + 1)
	if len(problems) == 0 {
		t.Fatalf("a cohort that ran %d wide against a bound of %d was reported clean", g.Peak(), bound)
	}
	if !strings.Contains(problems[0], "not holding") {
		t.Errorf("problems = %q, want the overrun named", problems)
	}
}

// And when it silently serializes, which is the failure no test inside the
// cohort can see.
func TestSerializedCohortIsReported(t *testing.T) {
	const bound = 4
	g := New(bound)
	// One at a time, enough of them that the cohort is unambiguously big enough
	// to have run wide: the shape it takes when `t.Parallel` is dropped from it.
	for range 2 * bound {
		g.acquire("")
		g.release()
	}
	if got := g.Peak(); got != 1 {
		t.Fatalf("peak = %d, want 1", got)
	}
	problems := g.problems(8)
	if len(problems) == 0 {
		t.Fatal("a cohort that never ran two tests at once was reported clean")
	}
	if !strings.Contains(problems[0], "serialized") {
		t.Errorf("problems = %q, want the serialization named", problems)
	}
}

// The three constraints a cohort cannot be blamed for: `-parallel` below the
// bound, too few tests for the width to mean anything, and an unreadable flag.
// Reporting any of them would make the check fire on `go test -parallel=1` or
// on `go test -run` selecting a handful of cases.
func TestNarrowerThanTheBoundIsCleanWhenItCouldNotBeWider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bound  int
		hold   int
		extra  int // further tests, one at a time, after the concurrent burst
		limit  int
		reason string
	}{
		{name: "at the bound", bound: 3, hold: 3, extra: 6, limit: 8},
		{name: "capped by -parallel", bound: 8, hold: 2, extra: 20, limit: 2},
		{name: "too few tests to fill it", bound: 8, hold: 2, limit: 8},
		{name: "-parallel unreadable", bound: 3, hold: 3, extra: 6, limit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New(tc.bound)
			holdAtOnce(t, g, tc.hold)
			for range tc.extra {
				g.acquire("")
				g.release()
			}
			if problems := g.problems(tc.limit); len(problems) != 0 {
				t.Errorf("problems = %q, want none", problems)
			}
		})
	}
}

// The `-parallel` value is readable from inside a test binary. If it ever
// stopped being, problems() would silently drop the serialization check for
// every cohort rather than fail — so the lookup is asserted on its own.
func TestParallelLimitIsReadable(t *testing.T) {
	if got := parallelLimit(); got < 1 {
		t.Errorf("parallelLimit() = %d, want the -parallel value; the serialization "+
			"half of every cohort's Check is now inert", got)
	}
}

// A member's subtest may not also take a slot: the parent holds its own until
// after the child finishes, so `bound` such parents would wait on each other
// forever. The refusal has to be a panic naming both tests, because the
// alternative is a `make check` that hangs with no output at all.
func TestNestedEntryIsRefusedRatherThanDeadlocked(t *testing.T) {
	g := New(2)
	// Recorded exactly as Enter records it, under this test's real name, so the
	// lookup under test is the real one and not a fixture's idea of nesting.
	g.acquire(t.Name())

	t.Run("inner", func(inner *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				inner.Error("a subtest of a cohort member was admitted; the cohort " +
					"deadlocks as soon as two such parents are in flight")
				return
			}
			if msg, _ := r.(string); !strings.Contains(msg, t.Name()) {
				inner.Errorf("panic = %v, want the enclosing test named", r)
			}
		}()
		g.Enter(inner)
	})
}

// A group whose subtests are the cohort members takes no slot itself, so its
// children are admitted rather than refused — and the whole table gets the
// cohort's width instead of one slot.
func TestAGroupsSubtestsAreAdmitted(t *testing.T) {
	const bound = 2
	g := New(bound)
	t.Cleanup(func() { g.Check(t) })

	t.Run("table", func(group *testing.T) {
		g.EnterGroup(group)
		for i := range 2 * bound {
			group.Run(fmt.Sprint(i), func(row *testing.T) {
				g.Enter(row)
				time.Sleep(20 * time.Millisecond)
			})
		}
	})
}

// A group nobody joined is the one way past the bound this package cannot see
// in its peak: the escaping test was never counted, so only the group itself
// can report it.
func TestAnEmptyGroupIsReported(t *testing.T) {
	g := New(4)
	g.groups = append(g.groups, "TestTableWhoseRowsForgotToJoin")
	// Enough real members that the width check is in force, so the empty group
	// is what this row is about rather than a cohort that never ran wide.
	holdAtOnce(t, g, 4)
	for range 4 {
		g.acquire("")
		g.release()
	}

	problems := g.problems(8)
	if len(problems) == 0 {
		t.Fatal("a group whose subtests never joined was reported clean; it ran parallel and ungated")
	}
	if !strings.Contains(problems[0], "TestTableWhoseRowsForgotToJoin") {
		t.Errorf("problems = %q, want the empty group named", problems)
	}
}

// A sibling whose name merely shares a prefix is not nested, and refusing it
// would be a false positive on a perfectly ordinary naming scheme.
func TestPrefixSiblingIsNotNested(t *testing.T) {
	g := New(2)
	g.acquire("TestOuter")
	for _, name := range []string{"TestOuterMost", "TestInner", "TestOuter"} {
		if got := g.enclosing(name); got != "" {
			t.Errorf("enclosing(%q) = %q, want none", name, got)
		}
	}
	if got := g.enclosing("TestOuter/inner"); got != "TestOuter" {
		t.Errorf("enclosing(%q) = %q, want TestOuter", "TestOuter/inner", got)
	}
}

func TestNewRefusesABoundBelowOne(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New(0) returned a gate; a cohort admitting nobody never finishes")
		}
	}()
	New(0)
}

// holdAtOnce takes n slots simultaneously and releases them together, so the
// gate's peak is n if it admitted them and less if it did not.
func holdAtOnce(t *testing.T, g *Gate, n int) {
	t.Helper()
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(n)
	for range n {
		go func() {
			defer done.Done()
			g.acquire("")
			arrived.Done()
			<-release
			g.release()
		}()
	}
	arrived.Wait()
	close(release)
	done.Wait()
}
