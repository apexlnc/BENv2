package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The sample unit is cited by name from supervise's comment and from the
// repeat-signal warning an operator reads at 2am. A citation to a file nobody
// checks is the shape this repo treats as a defect — it survives the file being
// renamed, moved, or edited into disagreeing with the code that points at it.
//
// So this asserts the settings the daemon's own behaviour depends on, and that
// they are the values the comments claim. It is not a lint of the whole unit:
// everything else in there is an operator's to change.

// unitSections parses the unit into section → directive → values.
//
// Parsed rather than string-matched, because the first version of this test
// matched substrings and passed against a unit whose StartLimit directives sat
// in [Service], where systemd logs them as an unknown key and ignores them. A
// directive in the wrong section is present and inert, and only a reader that
// knows about sections can tell those two apart.
func unitSections(t *testing.T, path string) map[string]map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the unit two comments in this package cite does not exist: %v", err)
	}
	out := map[string]map[string][]string{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			if out[section] == nil {
				out[section] = map[string][]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Errorf("unparseable unit line outside any section: %q", line)
			continue
		}
		out[section][key] = append(out[section][key], value)
	}
	return out
}

func TestTheSampleUnitSaysWhatTheDaemonReliesOn(t *testing.T) {
	unit := unitSections(t, filepath.Join("..", "..", "deploy", "ben.service"))

	only := func(section, key string) string {
		t.Helper()
		v := unit[section][key]
		switch len(v) {
		case 0:
			return ""
		case 1:
			return v[0]
		default:
			t.Errorf("[%s] %s is set %d times; a reader takes the first and systemd takes the last", section, key, len(v))
			return v[len(v)-1]
		}
	}

	// KillMode=mixed. systemd's default is control-group, which SIGTERMs BEN and
	// every agent it launched at once — exactly what §9.8's ordered drain exists
	// to avoid. A unit that omits this quietly gets the default.
	if got := only("Service", "KillMode"); got != "mixed" {
		t.Errorf("KillMode = %q, want mixed; otherwise systemd signals the agents behind BEN's back (SPEC §9.8)", got)
	}

	// TimeoutStopSec, which supervise names as the *only* bound on a drain it
	// deliberately does not bound itself.
	raw := only("Service", "TimeoutStopSec")
	if raw == "" {
		t.Fatal("the unit sets no TimeoutStopSec, so the drain supervise calls unbounded really is unbounded")
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("TimeoutStopSec = %q, which this test cannot compare against systemd's default", raw)
	}
	// systemd's own default is 90s. A sample that did not raise it would leave an
	// operator with the default under a comment claiming this is the knob.
	if secs <= 90 {
		t.Errorf("TimeoutStopSec = %ds, at or below systemd's default; an interrupted agent gets no more time than a web server", secs)
	}

	// The start-rate limit belongs in [Unit]. systemd moved it there in v229; in
	// [Service] it is logged as an unknown key and ignored, which is a rate limit
	// that silently is not one — and BEN's startup refusals (§5.7) do not fix
	// themselves, so the restart loop it guards against is the expected failure.
	for _, key := range []string{"StartLimitIntervalSec", "StartLimitBurst"} {
		if len(unit["Service"][key]) > 0 {
			t.Errorf("%s is in [Service], where systemd ignores it; it belongs in [Unit]", key)
		}
		if len(unit["Unit"][key]) == 0 {
			t.Errorf("%s is not set in [Unit]", key)
		}
	}

	// ProtectHome would break the agents' own session storage, and break it
	// silently: the run succeeds and the continuation token it minted then
	// resumes nothing, which disables §9.6's continuation track with no error
	// anywhere. Reinstating it means relocating that state first, which is why
	// the unit carries the recipe in a comment rather than the directive.
	if got := only("Service", "ProtectHome"); got != "" {
		t.Errorf("ProtectHome=%s is set; agents keep resumable session state under the service user's home, and a read-only one disables §9.6's continuation track without failing", got)
	}

	if got := only("Service", "ExecStart"); !strings.Contains(got, "ben run") {
		t.Errorf("ExecStart = %q, which does not start `ben run`", got)
	}

	// SPEC §10.1 is normative and says a deployment MUST NOT arrive in
	// risk-accepted mode "by default or by omission". This unit *is* in that
	// mode — the agent inherits the service identity (§6.7) and so can read the
	// tracker credential out of EnvironmentFile — so it has to say so, and the
	// acknowledgement has to be deliberate.
	//
	// Both halves are asserted. The prose alone would be the omission the spec
	// forbids; the gate alone would refuse to start without saying why.
	if got := only("Service", "ExecStartPre"); got != "/bin/false" {
		t.Errorf("ExecStartPre = %q; the sample must refuse to start until an operator removes the §10.1 acknowledgement gate", got)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "ben.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"§10.1", "risk-accepted mode", "NOT SATISFIED"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the unit does not state its §10.1 mode (missing %q); a deployment must not arrive in it by omission", want)
		}
	}
}
