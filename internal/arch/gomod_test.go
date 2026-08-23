package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go version floor is minor-level by decision (#110), with no separate
// toolchain preference. A patch-level `go` directive costs every contributor
// whose toolchain sits below it a whole-toolchain download under
// GOTOOLCHAIN=auto, and a hard refusal under =local — sandboxed, offline, or
// locked-down machines. The policy is enforced here rather than left to review
// because the artifact arrives silently and CI cannot report it: `go mod init`
// writes the running toolchain's patch level, nothing about the resulting file
// looks wrong, and the one machine that always satisfies the pin is the one
// that installs exactly what the pin names.
//
// The rule is a shape, not a version: raising the floor to `go 1.27` is
// ordinary, naming `go 1.27.3` is the thing that needs an argument.

// moduleDirective returns the value named by a top-level go.mod directive.
//
// Hand-parsed: golang.org/x/mod/modfile would read it properly, but it is an
// indirect dependency here, and promoting one for two fixed-shape directives
// is not the deliberate widening AGENTS.md rule 3 asks for. Neither `go` nor
// `toolchain` can appear in a parenthesized block, so line scanning sees
// everything a real parser would.
func moduleDirective(gomod, name string) (string, bool) {
	for line := range strings.Lines(gomod) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}
		if before, _, found := strings.Cut(line, "//"); found {
			line = strings.TrimSpace(before)
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == name {
			return fields[1], true
		}
	}
	return "", false
}

func goDirective(gomod string) (string, bool) {
	return moduleDirective(gomod, "go")
}

// isMinorLevel reports whether v is exactly MAJOR.MINOR — the shape every
// patch release of that minor satisfies. Anything longer (1.26.1) or
// pre-release (1.26rc1) names one toolchain and turns the rest away.
func isMinorLevel(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return false
		}
	}
	return true
}

func TestGoDirectiveIsMinorLevel(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "go.mod")
	gomod, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	version, found := goDirective(string(gomod))
	if !found {
		t.Fatalf("%s: no `go` directive found", path)
	}
	if !isMinorLevel(version) {
		t.Errorf("%s: `go %s` names one toolchain; want MAJOR.MINOR, the shape every patch release of that "+
			"minor satisfies. A narrower floor makes GOTOOLCHAIN=auto download a toolchain and GOTOOLCHAIN=local "+
			"refuse, and CI cannot see either (#110). If a specific patch is genuinely required, say which fix "+
			"and why in go.mod and AGENTS.md, and change this test with it.",
			path, version)
	}
}

func TestGoModHasNoToolchainDirective(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "go.mod")
	gomod, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if toolchain, found := moduleDirective(string(gomod), "toolchain"); found {
		t.Errorf("%s: `toolchain %s` adds a preferred toolchain even though pinned setup-go v5 and "+
			"GOTOOLCHAIN=local ignore it, while GOTOOLCHAIN=auto may download it (#110). The decision is "+
			"to carry no toolchain preference; if one becomes necessary, say which compiler or runtime fix "+
			"requires it in go.mod and AGENTS.md, and change this test with it.", path, toolchain)
	}
}

func followsGoModPolicy(gomod string) bool {
	version, found := goDirective(gomod)
	_, hasToolchain := moduleDirective(gomod, "toolchain")
	return found && isMinorLevel(version) && !hasToolchain
}

// The predicate's negative control: driven by the real go.mod alone, the test
// above passes just as happily if either directive scan silently misses its
// target or the shape predicate accepts everything.
func TestGoModPolicyParsing(t *testing.T) {
	for _, tc := range []struct {
		name               string
		gomod              string
		wantVersion        string
		wantFound          bool
		wantMinorLevel     bool
		wantToolchain      string
		wantToolchainFound bool
		wantPolicy         bool
	}{
		{
			name:           "minor-level directive",
			gomod:          "module example.com/m\n\ngo 1.26\n",
			wantVersion:    "1.26",
			wantFound:      true,
			wantMinorLevel: true,
			wantPolicy:     true,
		},
		{
			// The artifact this rule exists to catch.
			name:        "patch-level directive",
			gomod:       "module example.com/m\n\ngo 1.26.1\n",
			wantVersion: "1.26.1",
			wantFound:   true,
		},
		{
			name:        "pre-release directive",
			gomod:       "module example.com/m\n\ngo 1.27rc1\n",
			wantVersion: "1.27rc1",
			wantFound:   true,
		},
		{
			// This repo's go.mod carries a comment about patch levels directly
			// above the directive; a scan that read comments would report the
			// prose instead of the line.
			name:           "commented-out directive is not the directive",
			gomod:          "module example.com/m\n\n// not this: go 1.26.1\ngo 1.26\n",
			wantVersion:    "1.26",
			wantFound:      true,
			wantMinorLevel: true,
			wantPolicy:     true,
		},
		{
			name:           "trailing comment is trimmed",
			gomod:          "module example.com/m\n\ngo 1.26 // floor\n",
			wantVersion:    "1.26",
			wantFound:      true,
			wantMinorLevel: true,
			wantPolicy:     true,
		},
		{
			name:               "toolchain directive violates policy",
			gomod:              "module example.com/m\n\ngo 1.26\n\ntoolchain go1.26.1\n",
			wantVersion:        "1.26",
			wantFound:          true,
			wantMinorLevel:     true,
			wantToolchain:      "go1.26.1",
			wantToolchainFound: true,
		},
		{
			name:  "no directive",
			gomod: "module example.com/m\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version, found := goDirective(tc.gomod)
			if found != tc.wantFound || version != tc.wantVersion {
				t.Fatalf("goDirective = (%q, %v), want (%q, %v)", version, found, tc.wantVersion, tc.wantFound)
			}
			if got := isMinorLevel(version); found && got != tc.wantMinorLevel {
				t.Errorf("isMinorLevel(%q) = %v, want %v", version, got, tc.wantMinorLevel)
			}
			toolchain, toolchainFound := moduleDirective(tc.gomod, "toolchain")
			if toolchainFound != tc.wantToolchainFound || toolchain != tc.wantToolchain {
				t.Errorf("moduleDirective(toolchain) = (%q, %v), want (%q, %v)",
					toolchain, toolchainFound, tc.wantToolchain, tc.wantToolchainFound)
			}
			if got := followsGoModPolicy(tc.gomod); got != tc.wantPolicy {
				t.Errorf("followsGoModPolicy = %v, want %v", got, tc.wantPolicy)
			}
		})
	}
}

// workflowLine returns the indentation and content of an active YAML line.
// YAML forbids tab indentation; this scanner intentionally handles only the
// fixed workflow shape whose policy it checks, not YAML in general.
func workflowLine(line string) (int, string, bool) {
	line = strings.TrimSuffix(line, "\r")
	content := strings.TrimLeft(line, " ")
	if content == "" || strings.HasPrefix(content, "#") {
		return 0, "", false
	}
	return len(line) - len(content), content, true
}

func workflowScalar(content, key string) (string, bool) {
	value, found := strings.CutPrefix(content, key+":")
	if !found {
		return "", false
	}
	value = strings.TrimSpace(value)
	if before, _, hasComment := strings.Cut(value, " #"); hasComment {
		value = strings.TrimSpace(before)
	}
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') ||
		(value[0] == '"' && value[len(value)-1] == '"')) {
		value = value[1 : len(value)-1]
	}
	return value, true
}

func workflowBlockEnd(lines []string, start, parentIndent int) int {
	for end := start; end < len(lines); end++ {
		indent, _, active := workflowLine(lines[end])
		if active && indent <= parentIndent {
			return end
		}
	}
	return len(lines)
}

func sequenceItem(content string) (string, int, bool) {
	if !strings.HasPrefix(content, "- ") {
		return "", 0, false
	}
	remainder := content[1:]
	field := strings.TrimLeft(remainder, " ")
	return field, 1 + len(remainder) - len(field), true
}

// setupGoInputs returns the active inputs for every setup-go step in every
// steps sequence. Both `uses` and `with` must be direct fields of the step;
// nested action inputs with the same names are not steps.
func setupGoInputs(workflow string) []map[string]string {
	lines := strings.Split(workflow, "\n")
	var setupSteps []map[string]string
	for i := 0; i < len(lines); i++ {
		stepsIndent, content, active := workflowLine(lines[i])
		stepsValue, isSteps := workflowScalar(content, "steps")
		if !active || !isSteps || stepsValue != "" {
			continue
		}

		stepsEnd := workflowBlockEnd(lines, i+1, stepsIndent)
		stepIndent := -1
		for j := i + 1; j < stepsEnd; j++ {
			indent, item, active := workflowLine(lines[j])
			if _, _, isItem := sequenceItem(item); active && isItem && (stepIndent < 0 || indent < stepIndent) {
				stepIndent = indent
			}
		}
		if stepIndent < 0 {
			i = stepsEnd - 1
			continue
		}

		for j := i + 1; j < stepsEnd; {
			indent, item, active := workflowLine(lines[j])
			firstField, fieldOffset, isItem := sequenceItem(item)
			if !active || !isItem || indent != stepIndent {
				j++
				continue
			}

			stepEnd := workflowBlockEnd(lines, j+1, stepIndent)
			fieldIndent := stepIndent + fieldOffset
			isSetupGo := false
			withLine := -1
			for k := j; k < stepEnd; k++ {
				fieldLineIndent, field, active := workflowLine(lines[k])
				if k == j {
					fieldLineIndent, field, active = fieldIndent, firstField, firstField != ""
				}
				if !active || fieldLineIndent != fieldIndent {
					continue
				}
				if uses, found := workflowScalar(field, "uses"); found && strings.HasPrefix(uses, "actions/setup-go@") {
					isSetupGo = true
				}
				if value, found := workflowScalar(field, "with"); found && value == "" {
					withLine = k
				}
			}
			if !isSetupGo {
				j = stepEnd
				continue
			}

			inputs := make(map[string]string)
			if withLine >= 0 {
				withEnd := workflowBlockEnd(lines, withLine+1, fieldIndent)
				if withEnd > stepEnd {
					withEnd = stepEnd
				}
				inputIndent := -1
				for k := withLine + 1; k < withEnd; k++ {
					indent, _, active := workflowLine(lines[k])
					if active && (inputIndent < 0 || indent < inputIndent) {
						inputIndent = indent
					}
				}
				for k := withLine + 1; k < withEnd; k++ {
					indent, input, active := workflowLine(lines[k])
					if !active || indent != inputIndent {
						continue
					}
					for _, key := range []string{"go-version-file", "go-version"} {
						if value, found := workflowScalar(input, key); found {
							inputs[key] = value
						}
					}
				}
			}
			setupSteps = append(setupSteps, inputs)
			j = stepEnd
		}
		i = stepsEnd - 1
	}
	return setupSteps
}

func ciResolvesGoVersionFromGoMod(workflow string) bool {
	setupSteps := setupGoInputs(workflow)
	if len(setupSteps) == 0 {
		return false
	}
	for _, inputs := range setupSteps {
		if inputs["go-version-file"] != "go.mod" {
			return false
		}
		if _, hasLiteral := inputs["go-version"]; hasLiteral {
			return false
		}
	}
	return true
}

// The directive governs CI only because the setup-go step actively resolves
// from it. A literal `go-version:` would leave the two free to drift — the same
// blind spot from the other side, and the reason the patch floor survived
// unnoticed from B01 to #110.
func TestCIResolvesGoVersionFromGoMod(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	workflow, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	setupSteps := setupGoInputs(string(workflow))
	if len(setupSteps) == 0 {
		t.Fatalf("%s: no active actions/setup-go step", path)
	}
	for i, inputs := range setupSteps {
		if got, active := inputs["go-version-file"]; !active {
			t.Errorf("%s: setup-go step %d has no active `go-version-file`; want `go-version-file: go.mod` (#110)",
				path, i+1)
		} else if got != "go.mod" {
			t.Errorf("%s: setup-go step %d has active `go-version-file: %s`; want `go-version-file: go.mod` (#110)",
				path, i+1, got)
		}
		if version, found := inputs["go-version"]; found {
			t.Errorf("%s: setup-go step %d has active `go-version: %s`, which pins CI independently of go.mod (#110)",
				path, i+1, version)
		}
	}
}

func TestCIWorkflowParsing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workflow string
		want     bool
	}{
		{
			name: "active setup-go input",
			workflow: "steps:\n" +
				"  - uses: actions/setup-go@v5\n" +
				"    with:\n" +
				"      go-version-file: go.mod\n",
			want: true,
		},
		{
			name: "quoted input",
			workflow: "steps:\n" +
				"  - name: setup\n" +
				"    uses: 'actions/setup-go@v5'\n" +
				"    with:\n" +
				"      go-version-file: \"go.mod\" # one source\n",
			want: true,
		},
		{
			name: "commented-out input",
			workflow: "steps:\n" +
				"  - uses: actions/setup-go@v5\n" +
				"    with:\n" +
				"      # go-version-file: go.mod\n",
		},
		{
			name: "input belongs to another step",
			workflow: "steps:\n" +
				"  - uses: actions/setup-go@v5\n" +
				"  - uses: example/other@v1\n" +
				"    with:\n" +
				"      go-version-file: go.mod\n",
		},
		{
			name: "literal version also active",
			workflow: "steps:\n" +
				"  - uses: actions/setup-go@v5\n" +
				"    with:\n" +
				"      go-version-file: go.mod\n" +
				"      go-version: 1.26.1\n",
		},
		{
			name: "every setup-go step is valid",
			workflow: "jobs:\n" +
				"  first:\n" +
				"    steps:\n" +
				"      - uses: actions/setup-go@v5\n" +
				"        with:\n" +
				"          go-version-file: go.mod\n" +
				"  second:\n" +
				"    steps:\n" +
				"      - uses: actions/setup-go@v5\n" +
				"        with:\n" +
				"          go-version-file: go.mod\n",
			want: true,
		},
		{
			name: "second setup-go step has literal version",
			workflow: "jobs:\n" +
				"  first:\n" +
				"    steps:\n" +
				"      - uses: actions/setup-go@v5\n" +
				"        with:\n" +
				"          go-version-file: go.mod\n" +
				"  second:\n" +
				"    steps:\n" +
				"      - uses: actions/setup-go@v5\n" +
				"        with:\n" +
				"          go-version: 1.26.1\n",
		},
		{
			name: "nested uses input is not a setup-go step",
			workflow: "steps:\n" +
				"  - uses: example/other@v1\n" +
				"    with:\n" +
				"      uses: actions/setup-go@v5\n" +
				"      go-version-file: go.mod\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ciResolvesGoVersionFromGoMod(tc.workflow); got != tc.want {
				t.Errorf("ciResolvesGoVersionFromGoMod = %v, want %v", got, tc.want)
			}
		})
	}
}
