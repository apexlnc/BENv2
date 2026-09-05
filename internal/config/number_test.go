package config

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// The numeric-spelling refusals (#242).
//
// Every case here is anchored on what **Load** does with a written file, not on
// what the schema declares, because the defect is a dependency behaviour: the
// schema says `int` and yaml.v3 silently truncates a float into it, so a test
// driven off the declaration agrees with the code and sees nothing. The one
// declaration-driven test below is the completeness half — it asserts the raw
// schema contains no field yaml.v3 could still coerce — and it is deliberately
// paired with, not a substitute for, the behavioural tables.

// withLimits writes a `limits:` block into validMinimal.
func withLimits(body string) string {
	return strings.Replace(validMinimal, "agent:", "limits:\n"+body+"agent:", 1)
}

// intSpellings is what YAML calls a number, and what this loader does with each
// where the schema declares an integer. The accepted column is the other half of
// the proof: a refusal table alone would pass on a loader that rejected every
// number.
var intSpellings = []struct {
	name   string
	spell  string
	want   int // when accepted
	refuse bool
}{
	{name: "plain integer", spell: "3", want: 3},
	{name: "underscored integer", spell: "2_0", want: 20},
	{name: "hexadecimal integer", spell: "0x10", want: 16},
	// The issue's case: `max_attempts: 2.7` loaded as 2.
	{name: "non-integral float", spell: "2.7", refuse: true},
	// Integral in value, float in spelling. Refused on the node's tag, like
	// `version: "1"` is refused with the right value in the wrong type — the
	// alternative rule ("accept a float that happens to be whole") would make
	// acceptance depend on the operator's rounding rather than on the type.
	{name: "integral float", spell: "2.0", refuse: true},
	{name: "exponent float", spell: "3e0", refuse: true},
	// `.nan` and `.inf` fail yaml.v3's own int conversion, but `-.inf` does not:
	// it decodes to math.MinInt64. One rule covers all three.
	{name: "nan", spell: ".nan", refuse: true},
	{name: "positive infinity", spell: ".inf", refuse: true},
	{name: "negative infinity", spell: "-.inf", refuse: true},
}

func TestLoadRefusesFloatSpellingsOfIntegerFields(t *testing.T) {
	for _, tc := range intSpellings {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Load(writeWorkflow(t, withLimits("  max_attempts: "+tc.spell+"\n")))
			if !tc.refuse {
				if err != nil {
					t.Fatalf("Load(%s): %v", tc.spell, err)
				}
				if got := def.Config.Limits.MaxAttempts; got != tc.want {
					t.Fatalf("max_attempts = %d, want %d", got, tc.want)
				}
				return
			}
			var nie *NonIntegerError
			if !errors.As(err, &nie) {
				t.Fatalf("Load(%s) = %v, want a *NonIntegerError", tc.spell, err)
			}
			if !errors.Is(err, ErrNonIntegerValue) {
				t.Errorf("refusal does not unwrap to ErrNonIntegerValue: %v", err)
			}
			if nie.Field != "limits.max_attempts" {
				t.Errorf("Field = %q, want limits.max_attempts", nie.Field)
			}
			if nie.Value != tc.spell {
				t.Errorf("Value = %q, want %q", nie.Value, tc.spell)
			}
			if nie.Line <= 0 {
				t.Errorf("Line = %d, want the line the value is written on", nie.Line)
			}
			if !strings.Contains(err.Error(), tc.spell) {
				t.Errorf("refusal does not quote the value it refused: %v", err)
			}
			// The refusal names the file, like every other one Load returns.
			var werr *WorkflowError
			if !errors.As(err, &werr) {
				t.Errorf("refusal does not name the workflow file: %v", err)
			}
		})
	}
}

// A float spelling is refused wherever an integer is declared, not only in
// `limits`. Each case names a section with a different resolver: the top-level
// version probe, the `substrate.airlock` block, and the `review` block.
func TestLoadRefusesFloatSpellingsAcrossSections(t *testing.T) {
	cases := []struct {
		name    string
		content string
		field   string
	}{
		{
			name:    "version",
			content: strings.Replace(validMinimal, "tracker:", "version: 1.5\ntracker:", 1),
			field:   "version",
		},
		{
			name:    "polling.interval_ms",
			content: strings.Replace(validMinimal, "agent:", "polling:\n  interval_ms: 1000.5\nagent:", 1),
			field:   "polling.interval_ms",
		},
		{
			name:    "hooks.timeout_ms",
			content: strings.Replace(validMinimal, "agent:", "hooks:\n  timeout_ms: 5000.5\nagent:", 1),
			field:   "hooks.timeout_ms",
		},
		{
			name:    "substrate.airlock.poll_wait_ms",
			content: strings.Replace(validAirlock, "    auth_source: airlock", "    auth_source: airlock\n    poll_wait_ms: 20000.5", 1),
			field:   "substrate.airlock.poll_wait_ms",
		},
		{
			name:    "review.round_cap",
			content: strings.Replace(validMinimal, "deployment:", "review:\n  enabled: true\n  round_cap: 2.5\ndeployment:", 1),
			field:   "review.round_cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRACKER_PAT", "tracker-secret")
			var nie *NonIntegerError
			_, err := Load(writeWorkflow(t, tc.content))
			if !errors.As(err, &nie) {
				t.Fatalf("Load = %v, want a *NonIntegerError", err)
			}
			if nie.Field != tc.field {
				t.Fatalf("Field = %q, want %q", nie.Field, tc.field)
			}
		})
	}
}

// The sharpest case in the report, and the reason a truncation is not merely
// imprecise: 0 is the *valid* "leave the profile's own window" spelling for both
// idle keys, so `0.9` and `-0.5` truncated to a value that reads as a deliberate
// choice. A daemon that crashed between attempts would then leave a sandbox and
// its volume allocated under a window the operator believes they configured.
func TestLoadRefusesTruncationIntoAValidDefaultSpelling(t *testing.T) {
	for _, key := range []string{"idle_suspend_ms", "delete_after_idle_ms"} {
		for _, spell := range []string{"0.9", "-0.5"} {
			t.Run(key+"="+spell, func(t *testing.T) {
				t.Setenv("TRACKER_PAT", "tracker-secret")
				content := strings.Replace(validAirlock,
					"    auth_source: airlock",
					"    auth_source: airlock\n    "+key+": "+spell, 1)
				var nie *NonIntegerError
				if _, err := Load(writeWorkflow(t, content)); !errors.As(err, &nie) {
					t.Fatalf("Load = %v, want a *NonIntegerError", err)
				}
				if want := "substrate.airlock." + key; nie.Field != want {
					t.Fatalf("Field = %q, want %q", nie.Field, want)
				}
			})
		}
	}
}

// The line a refusal quotes is the line in the **file**, which is the one an
// operator's editor uses. The front matter reaches the decoder with its `---`
// delimiter already removed, so a node line taken as written points at the key
// above the one that is wrong — and every key here is one line apart.
func TestNumericRefusalPointsAtTheFileLine(t *testing.T) {
	content := `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
limits:
  max_turns: 4
  max_attempts: 2.7
agent:
  kind: claude-code
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`
	lines := strings.Split(content, "\n")
	want := 0
	for i, l := range lines {
		if strings.Contains(l, "max_attempts") {
			want = i + 1 // 1-based
		}
	}
	if want == 0 {
		t.Fatal("fixture no longer writes max_attempts")
	}

	var nie *NonIntegerError
	_, err := Load(writeWorkflow(t, content))
	if !errors.As(err, &nie) {
		t.Fatalf("Load = %v, want a *NonIntegerError", err)
	}
	if nie.Line != want {
		t.Errorf("Line = %d, want %d (%q)", nie.Line, want, lines[want-1])
	}
}

// limits.max_cost_usd is the only float field the schema declares today, and the
// one knob bounding attacker-driven token spend. NaN passed its "must be
// positive when set" guard, both consumers' `> 0` gates, and was printed by
// `config effective` as though set; +Inf reached the child argv as a budget.
func TestLoadRefusesNonFiniteFloats(t *testing.T) {
	cases := []struct {
		name   string
		spell  string
		want   float64 // when accepted
		refuse bool
	}{
		{name: "plain float", spell: "2.5", want: 2.5},
		{name: "integer spelling of a float field", spell: "3", want: 3},
		{name: "nan", spell: ".nan", refuse: true},
		{name: "capitalized nan", spell: ".NaN", refuse: true},
		{name: "positive infinity", spell: ".inf", refuse: true},
		{name: "negative infinity", spell: "-.inf", refuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Load(writeWorkflow(t, withLimits("  max_cost_usd: "+tc.spell+"\n")))
			if !tc.refuse {
				if err != nil {
					t.Fatalf("Load(%s): %v", tc.spell, err)
				}
				if def.Config.Limits.MaxCostUSD == nil || *def.Config.Limits.MaxCostUSD != tc.want {
					t.Fatalf("max_cost_usd = %v, want %v", def.Config.Limits.MaxCostUSD, tc.want)
				}
				return
			}
			var nfe *NonFiniteError
			if !errors.As(err, &nfe) {
				t.Fatalf("Load(%s) = %v, want a *NonFiniteError", tc.spell, err)
			}
			if !errors.Is(err, ErrNonFiniteValue) {
				t.Errorf("refusal does not unwrap to ErrNonFiniteValue: %v", err)
			}
			if nfe.Field != "limits.max_cost_usd" {
				t.Errorf("Field = %q, want limits.max_cost_usd", nfe.Field)
			}
			if nfe.Value != tc.spell {
				t.Errorf("Value = %q, want %q", nfe.Value, tc.spell)
			}
		})
	}
}

// The value rule, asked of validate directly. Anchored away from Load
// deliberately: validate takes a Config so it can be called on one nobody read
// off disk, and `*v <= 0` is false for NaN — written the obvious way, the cap
// would accept it there and be inert at every `> 0` downstream.
func TestValidateRefusesNonFiniteMaxCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
	}{
		{"nan", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Load(writeWorkflow(t, validMinimal))
			if err != nil {
				t.Fatal(err)
			}
			cfg := def.Config
			v := tc.v
			cfg.Limits.MaxCostUSD = &v

			var verr *ValidationError
			if err := validate(&cfg); !errors.As(err, &verr) {
				t.Fatalf("validate = %v, want a *ValidationError", err)
			} else if verr.Field != "limits.max_cost_usd" || !strings.Contains(verr.Msg, "finite") {
				t.Fatalf("refused %q: %s", verr.Field, verr.Msg)
			}
		})
	}
}

// A mistyped version must be diagnosed as what it is. The probe used to discard
// its own error and read the zero value yaml.v3 left behind, so a quoted or
// typo'd value refused with "declares config version 0 … upgrade ben to use this
// file" — a fabricated version, and an instruction to replace the binary.
func TestLoadDiagnosesAMistypedVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spell string
	}{
		{"quoted integer", `"1"`},
		{"word", "one"},
		{"empty string", `""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeWorkflow(t, strings.Replace(validMinimal, "tracker:", "version: "+tc.spell+"\ntracker:", 1)))
			if err == nil {
				t.Fatal("Load accepted a mistyped version")
			}
			var uve *UnsupportedVersionError
			if errors.As(err, &uve) {
				t.Fatalf("diagnosed as an unsupported version %d: %v", uve.Version, err)
			}
			if strings.Contains(err.Error(), "upgrade ben") {
				t.Fatalf("tells the operator to upgrade ben over a type error: %v", err)
			}
			if !strings.Contains(err.Error(), "cannot unmarshal") {
				t.Fatalf("does not describe the type error: %v", err)
			}
		})
	}
}

// A version this daemon does not understand still refuses as one, and still
// refuses *before* strict key validation (SPEC §5.2.1). Preserving the probe's
// error must not cost the ordering the probe exists for.
func TestLoadStillRefusesAFutureVersionFirst(t *testing.T) {
	content := strings.Replace(validMinimal, "tracker:", "version: 2\nfuture_feature: {}\ntracker:", 1)
	var uve *UnsupportedVersionError
	if _, err := Load(writeWorkflow(t, content)); !errors.As(err, &uve) || uve.Version != 2 {
		t.Fatalf("want UnsupportedVersionError{2}, got %v", err)
	}
}

// The completeness half, and the reason it is anchored on the declaration while
// everything above is anchored on behaviour: the tables prove the fields they
// name refuse a float, and cannot see a numeric key added later as a bare `int`.
// This can, and it is the only assertion here that a new key inherits the
// refusal by construction rather than by somebody remembering a table.
func TestRawSchemaDeclaresNoCoercibleNumericField(t *testing.T) {
	intType, floatType := reflect.TypeOf(yamlInt(0)), reflect.TypeOf(yamlFloat(0))
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem(), path)
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if typ != intType {
				t.Errorf("%s is %s: yaml.v3 truncates a float spelling into it silently — declare it yamlInt", path, typ)
			}
		case reflect.Float32, reflect.Float64:
			if typ != floatType {
				t.Errorf("%s is %s: yaml.v3 admits .nan and .inf into it — declare it yamlFloat", path, typ)
			}
		}
	}
	walk(reflect.TypeOf(rawConfig{}), "rawConfig")
}

// The two projections the resolver uses. Float returns a fresh pointer because
// the config's own pointer is its "is the cap set at all?" answer, and one
// aliasing the raw document would let a later decode edit a resolved Config.
func TestNumericProjections(t *testing.T) {
	n := yamlInt(7)
	if n.Int() != 7 {
		t.Errorf("Int() = %d, want 7", n.Int())
	}
	f := yamlFloat(1.5)
	got := f.Float()
	if got == nil || *got != 1.5 {
		t.Fatalf("Float() = %v, want 1.5", got)
	}
	*got = 9
	if f != 1.5 {
		t.Errorf("Float() aliases the decoded field: %v", f)
	}
	var absent *yamlFloat
	if absent.Float() != nil {
		t.Error("an absent float field must project to nil, which is what disables the cap")
	}
}

// The unnamed fallback. locateNumeric names the key by matching the node's
// position in a re-parse, and the one case it cannot answer — a value reached
// through an alias, whose position is the anchor's — must still say where to
// look rather than refuse anonymously.
func TestNumericRefusalWithoutAFieldStillSaysWhere(t *testing.T) {
	for _, err := range []error{
		&NonIntegerError{Value: "2.7", Line: 12},
		&NonFiniteError{Value: ".nan", Line: 12},
	} {
		msg := err.Error()
		if !strings.Contains(msg, "line 12") {
			t.Errorf("%v does not say where to look", err)
		}
		if strings.Contains(msg, "invalid :") || strings.Contains(msg, "invalid (") {
			t.Errorf("%v reads as a refusal with an empty field", err)
		}
	}
}

// yaml.v3 decodes an alias through its anchor node, including the anchor's
// position. That position belongs to the provider's opaque data below, not to
// limits.max_attempts; reporting its path would send the operator to a valid
// field while hiding the schema field that consumed the bad value.
func TestNumericAliasDoesNotBlameTheAnchorField(t *testing.T) {
	content := `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    harmless: &bad 2.7
  required_labels: ["ben"]
limits:
  max_attempts: *bad
agent:
  kind: claude-code
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`
	_, err := Load(writeWorkflow(t, content))
	var nie *NonIntegerError
	if !errors.As(err, &nie) {
		t.Fatalf("Load = %v, want a *NonIntegerError", err)
	}
	if nie.Field != "" {
		t.Errorf("Field = %q, want no field rather than the anchor's path", nie.Field)
	}
	if nie.Line != 6 {
		t.Errorf("Line = %d, want anchor line 6", nie.Line)
	}
	if got := err.Error(); strings.Contains(got, "tracker.provider.harmless") {
		t.Errorf("refusal blames the anchor field: %v", err)
	} else if !strings.Contains(got, "integer field at line 6") {
		t.Errorf("refusal does not retain the actionable anchor location: %v", err)
	}
}
