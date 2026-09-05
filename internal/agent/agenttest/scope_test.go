package agenttest

import (
	"slices"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// Which of these cases are local *by subject* rather than by accident (#192).
//
// The whole suite is local by construction — every case re-execs this binary as a
// harness and drives a real child — so "runs a process" separates nothing. What a
// remote-substrate author needs is narrower and is a judgement: which cases
// assert a fact a POSIX process on this host has and a foreign process does not.
// Those can never be asked of a backend, and a fake that answered them would be
// inventing a guarantee no real backend makes (AGENTS.md, Conventions).
//
// The universal remainder — the outcomes every substrate owes — is stated once in
// internal/agent/runnertest and run unchanged by the local fake (internal/fake)
// and the remote one (internal/remote/remotetest). This file is the other half of
// that separation: it names what stayed behind, and why.
//
// # What is anchored and what is not
//
// The map is a judgement and is deliberately a **superset** of what a scan can
// see. A case whose subject is the signal ladder reaches the process group
// through the adapter's own code, not through an identifier in this package, so
// no source scan will ever find it — and leaving it undeclared for that reason
// would be the wrong way round.
//
// So one direction is anchored and it is the one that erodes silently: a case
// whose *source* reaches a local operating-system fact must be declared here,
// checked against the source rather than against the list
// (TestEveryCaseReachingALocalFactIsDeclared). The converse — that everything
// declared is genuinely local — is a claim this file makes and a reviewer checks,
// which is why each entry carries the fact rather than a bare flag.

// localFact is why a case is local-only. Values rather than a bool, because
// "local" on its own decays into "nobody has looked at it": the next reader has
// to be able to tell a pid assertion from an argv assertion without opening the
// case.
type localFact string

const (
	// processGroup: the case samples, signals, or reasons about a POSIX process
	// group. A backend answers about a run, and only through domain quiet
	// (remote.Status) — signal delivery is not termination there either, but the
	// evidence is a phase rather than ESRCH.
	processGroup localFact = "asserts a pid, a process group, or the signal ladder over one"
	// childIO: SPEC §7.6's surface — the child's argv, environment, cwd or stdin.
	// A remote adapter composes an invocation and hands it to a backend; what
	// arrives in the process is asserted against that backend, not here.
	childIO localFact = "asserts the child's argv, environment, cwd or stdin"
	// hostFiles: the case reads what the harness wrote on this host — the
	// retained transcript or prompt of SPEC §10.3.
	hostFiles localFact = "asserts a file the harness wrote on the daemon's own host"
	// hostHarness: the case is about finding and binding a harness binary on this
	// host's PATH (SPEC §7.7's readiness).
	hostHarness localFact = "asserts a harness binary on the daemon's own PATH"
)

// localOnlyCases names every case whose subject is one of the facts above.
var localOnlyCases = map[string]localFact{
	// The process group (SPEC §7.5, §9.8, #79).
	"DefaultSignalReachesDescendants":                processGroup,
	"LivenessKillReachesDescendantsBeforePublishing": processGroup,
	"ProbeObservesWithoutTouchingTheGroup":           processGroup,
	"ProcessKilledAfterTerminalEvent":                processGroup,
	"ReadyLeavesNoOrphanedProbeChild":                processGroup,
	"StallKillUnconfirmedWhenGroupSurvives":          processGroup,
	"StopAfterExitIsConfirmed":                       processGroup,
	"StopCleansAGroupThatOutlivedTheProcess":         processGroup,
	"StopEscalatesToSIGKILL":                         processGroup,
	"StopUnconfirmedWhenProbeIsDenied":               processGroup,
	"StopUnconfirmedWhenSignalsDoNotLand":            processGroup,
	"TimeoutKillUnconfirmedWhenGroupSurvives":        processGroup,
	"UnconfirmedInterruptLeavesTheStreamAlone":       processGroup,

	// The child's I/O surface (SPEC §7.6).
	"AFailedPublishMintNeverLaunchesTheAgent":             childIO,
	"ArgvAndChildEnvAudit":                                childIO,
	"ModelDeclaredIsModelLaunched":                        childIO,
	"PromptOnStdinAndCwdIsWorkspace":                      childIO,
	"PublishCredentialDeadlineCoversTheAttempt":           childIO,
	"PublishCredentialReachesItsVariableAndNotArgv":       childIO,
	"ReadyProbeEnvironmentIsRestricted":                   childIO,
	"StartRejectsRunSpecEnvOutsideNamespace":              childIO,
	"UnredactableForwardedValueRefusedAtReady":            childIO,
	"UnredactableForwardedValueRefusedAtStart":            childIO,
	"UnresolvablePublishCredentialRefusedAtReadyAndStart": childIO,

	// What the harness wrote here (SPEC §10.3).
	"NoStreamIsCrashedWithStderrInTranscript":      hostFiles,
	"PromptIsRetainedBesideTheTranscript":          hostFiles,
	"PublishCredentialIsRedactedFromTheTranscript": hostFiles,
	"TranscriptIsCompleteWithoutAConsumer":         hostFiles,
	"TranscriptRedactsCredentialValues":            hostFiles,
	"TranscriptRetainsRawStream":                   hostFiles,
	"TranscriptSinkSeesWholeLines":                 hostFiles,

	// The harness binary on this host (SPEC §7.7).
	"BinaryInstalledAfterNewIsFoundByReady": hostHarness,
	"BoundBinarySurvivesPathChange":         hostHarness,
	"RelativeBinaryIsBoundAbsolutely":       hostHarness,
	"StartMissingBinaryFailsBeforeHandle":   hostHarness,
	"StructuralIsPureAndNeedsNoHarness":     hostHarness,
}

// localMarkers are the operating-system facts a scan *can* see, and they are the
// mirror image of internal/agent/runnertest's forbidden list: what is refused
// there must be declared here.
//
// Deliberately narrow. A marker over filesystem calls was tried and removed: the
// fixtures write temporary directories for every case, so it matched the whole
// suite and separated nothing — it would have reported the harness setup rather
// than the assertion.
var localMarkers = []partest.Marker{
	{
		Name:  "process identity",
		Calls: []string{"Getpid", "Getppid", "Getpgid", "Setpgid", "FindProcess"},
		Funcs: []string{"aliveNow"},
		Why:   string(processGroup),
	},
	{
		Name:  "child I/O surface",
		Calls: []string{"Setenv", "Getenv", "LookupEnv", "Environ", "Unsetenv", "Getwd", "Chdir"},
		Why:   string(childIO),
	},
}

// A case that reaches a local operating-system fact is declared local-only.
//
// The scan is the anchor and the map is the claim, in that order: this fails when
// somebody adds a case asserting a pid or a child environment variable and does
// not say so, which is the direction that erodes the separation without anyone
// noticing.
func TestEveryCaseReachingALocalFactIsDeclared(t *testing.T) {
	src, byCase := suiteSource(t)

	for _, tc := range conformanceCases {
		fn := byCase[tc.name]
		for _, m := range localMarkers {
			if !src.Carries(fn, m) {
				continue
			}
			if _, ok := localOnlyCases[tc.name]; !ok {
				t.Errorf("case %q (%s) reaches a %s — it %s, so it is local-only and cannot be "+
					"asked of a remote substrate. Declare it in localOnlyCases.",
					tc.name, fn, m.Name, m.Why)
			}
		}
	}
}

// Every name in the map is a case that exists.
//
// A name that no longer matches a case sends the next reader looking for
// something that is not there — the same rot internal/arch's doc-map check exists
// for, and the reason a judgement recorded as a list needs at least this much.
func TestLocalOnlyDeclarationsNameRealCases(t *testing.T) {
	_, byCase := suiteSource(t)
	for name, fact := range localOnlyCases {
		if _, ok := byCase[name]; !ok {
			t.Errorf("localOnlyCases names %q (%s), which is not a case in this suite", name, fact)
		}
	}
}

// The scan is a real detector rather than one that finds nothing.
func TestEveryLocalMarkerFiresSomewhere(t *testing.T) {
	src, byCase := suiteSource(t)

	for _, m := range localMarkers {
		fired := false
		for _, tc := range conformanceCases {
			if src.Carries(byCase[tc.name], m) {
				fired = true
				break
			}
		}
		if !fired {
			t.Errorf("marker %q matched no case at all: the scan behind "+
				"TestEveryCaseReachingALocalFactIsDeclared is inert, and the local/universal "+
				"separation it anchors is now unchecked", m.Name)
		}
	}
}

// Both scopes are occupied. A suite where every case is local-only means the
// universal contract had nothing to be extracted from; one where none is means
// the classification stopped distinguishing anything.
func TestBothScopesAreOccupied(t *testing.T) {
	rest := 0
	for _, tc := range conformanceCases {
		if _, ok := localOnlyCases[tc.name]; !ok {
			rest++
		}
	}
	if rest == 0 {
		t.Error("every case is declared local-only; nothing is left for the universal contract")
	}
	if len(localOnlyCases) == 0 {
		t.Error("no case is declared local-only; the separation distinguishes nothing")
	}
	t.Logf("scopes: %d local-only, %d whose subject is not a local operating-system fact "+
		"(the universal outcomes among them are asserted in internal/agent/runnertest)",
		len(localOnlyCases), rest)
}

// Every declared fact is one of the four named constants, so a fifth reason has
// to be written down rather than spelled inline in one entry.
func TestEveryDeclaredFactIsANamedConstant(t *testing.T) {
	known := []localFact{processGroup, childIO, hostFiles, hostHarness}
	for name, fact := range localOnlyCases {
		if !slices.Contains(known, fact) {
			t.Errorf("case %q declares the fact %q, which is not one of the named constants", name, fact)
		}
	}
}
