package partest

import (
	"fmt"
	"slices"
	"sort"
)

// DefaultMarkers are the facts that keep a test out of a bounded cohort in the
// suites this repository runs against real processes.
//
// They are deliberately coarse. Each one is a *reason a machine can see*, which
// is what makes the classification checkable at all — but no list of them makes
// a test parallel-safe. What they do is stop the classification from drifting:
// a test that acquires a `t.Setenv`, a deadline or a fan-out after joining a
// cohort fails, rather than becoming a flake somebody chases six months later.
func DefaultMarkers() []Marker {
	return []Marker{
		{
			Name:  "t.Setenv/t.Chdir",
			Calls: []string{"Setenv", "Chdir"},
			Why: "it mutates state the whole test binary shares. The testing package enforces " +
				"this too — t.Setenv after t.Parallel panics — so a cohort membership here is a " +
				"panic waiting for the next run",
		},
		{
			Name:  "elapsed-time assertion",
			Calls: []string{"Since"},
			Why:   "an elapsed-time assertion measures the machine as much as the code",
		},
		{
			Name:  "context deadline",
			Calls: []string{"WithTimeout", "WithDeadline"},
			Why: "a case that gives its subject a deadline is asserting what happens when that " +
				"deadline expires on a machine whose load it does not control",
		},
		{
			Name:  "signal to another process",
			Calls: []string{"Kill"},
			Why: "polling whether a process is still there is an ordering assertion whose margin " +
				"may not be spent on neighbours",
		},
		{
			Name: "a timeout the case waits out",
			Keys: []string{"Timeout"},
			Why:  "the case spends a configured window, and a loaded machine spends a different one",
		},
		{
			Name: "its own concurrent fan-out",
			Go:   true,
			Why: "the case already runs several things at once, so a cohort would multiply its " +
				"fan-out rather than bound it",
		},
	}
}

// CohortAudit holds one package's test files to a complete policy: every
// top-level test is in the cohort, or carries a marker that keeps it out, or is
// exempt for a reason somebody wrote down.
//
// The completeness is the point. A cohort maintained as "whoever remembered to
// opt in" decays into a serial suite one new test at a time, and nothing fails
// while it does — the only symptom is `make check` getting slower again, which
// is not a failure anyone is shown.
type CohortAudit struct {
	// Dir is the package directory to read. "." for the package under test.
	Dir string
	// Join is the function a test calls to join the cohort. Group, when set, is
	// the one a table-driven parent calls instead, its subtests being the
	// members; a test calling either counts as accounted for.
	Join  string
	Group string
	// Markers keep a test out. Each must match something, or the scan behind
	// this audit has gone inert.
	Markers []Marker
	// Exempt names tests that stay out for a reason no marker can see, mapped
	// to that reason. An entry that stops being needed is reported: a stale
	// exemption is how a test that could rejoin the cohort never does.
	Exempt map[string]string
}

// Problems returns every way the package departs from the policy, or an error
// if its source cannot be read.
func (a CohortAudit) Problems() ([]string, error) {
	src, err := ParseSource(a.Dir, TestFiles)
	if err != nil {
		return nil, err
	}
	if !src.Declares(a.Join) {
		return nil, fmt.Errorf("this package declares no %s: the audit cannot tell "+
			"cohort members from serial tests", a.Join)
	}

	joins := []string{a.Join}
	if a.Group != "" {
		if !src.Declares(a.Group) {
			return nil, fmt.Errorf("this package declares no %s", a.Group)
		}
		joins = append(joins, a.Group)
	}
	joinMarker := Marker{Name: a.Join, Funcs: joins}
	// A test may only become parallel through the configured helpers. Calling
	// testing.T.Parallel directly bypasses the gate entirely, and a marker would
	// otherwise make that test look deliberately serial: joined=false and
	// blocked=true. Match the simple selector name like the other source markers;
	// a false positive fails loudly, while a missed call silently removes the
	// bound this audit exists to enforce.
	directParallel := Marker{Name: "t.Parallel", Calls: []string{"Parallel"}}
	var out []string
	usedExempt := map[string]bool{}
	firedMarker := map[string]bool{}

	for _, test := range src.TestFunctions() {
		joined := src.Carries(test, joinMarker)
		if src.Carries(test, directParallel) {
			out = append(out, fmt.Sprintf(
				"%s calls t.Parallel directly instead of %s: it runs outside the cohort's bound",
				test, a.Join))
		}
		reason, exempt := a.Exempt[test]
		if exempt {
			usedExempt[test] = true
		}

		blocked := false
		for _, m := range a.Markers {
			if !src.Carries(test, m) {
				continue
			}
			firedMarker[m.Name] = true
			blocked = true
			if joined {
				out = append(out, fmt.Sprintf(
					"%s joins the %s cohort but uses %s; %s", test, a.Join, m.Name, m.Why))
			}
		}

		switch {
		case exempt && joined:
			out = append(out, fmt.Sprintf(
				"%s is exempt (%q) yet joins the %s cohort; the exemption or the call is wrong",
				test, reason, a.Join))
		case exempt && blocked:
			out = append(out, fmt.Sprintf(
				"%s is exempt (%q) and also carries a marker; the marker already keeps it out, "+
					"so the exemption is noise", test, reason))
		case !joined && !blocked && !exempt:
			out = append(out, fmt.Sprintf(
				"%s is neither in the %s cohort nor kept out by anything: call %s(t), or record "+
					"why it must stay serial in the audit's Exempt map", test, a.Join, a.Join))
		}
	}

	for name := range a.Exempt {
		if !usedExempt[name] {
			out = append(out, fmt.Sprintf(
				"the audit exempts %q, which this package no longer declares; a stale exemption "+
					"hides the next test of that name from the policy", name))
		}
	}
	// Non-vacuity, and only this much of it. A scan pointed at the wrong
	// directory, or one whose parser stopped matching, reports every test clean
	// — indistinguishable from a compliant package — so at least one marker has
	// to have found something. Demanding that *every* marker fire would be the
	// stronger check and is the wrong one here: the set is shared across
	// packages, and a package with no test that waits out a deadline is not a
	// package whose audit has broken. What each kind of marker matches is
	// established against a fixture instead, in this package's own tests.
	if len(firedMarker) == 0 {
		out = append(out, fmt.Sprintf(
			"none of the %d markers matched any of the %d tests in %s: the scan has found "+
				"nothing, which is what it would report if it had stopped looking",
			len(a.Markers), len(src.TestFunctions()), a.Dir))
	}

	sort.Strings(out)
	return slices.Clip(out), nil
}
