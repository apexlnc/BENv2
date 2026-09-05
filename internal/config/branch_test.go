package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func withBaseBranch(fragment string) string {
	return strings.Replace(validMinimal, "agent:", "workspace:\n  "+fragment+"\nagent:", 1)
}

func TestWorkspaceBaseBranchLoadAndEffectiveOutput(t *testing.T) {
	t.Run("omitted selects the repository default", func(t *testing.T) {
		def, err := Load(writeWorkflow(t, validMinimal))
		if err != nil {
			t.Fatal(err)
		}
		if def.Config.Workspace.BaseBranch != "" {
			t.Fatalf("BaseBranch = %q, want the omission sentinel", def.Config.Workspace.BaseBranch)
		}
		if got := def.Provenance["workspace.base_branch"].Source; got != SourceDefault {
			t.Fatalf("provenance = %q, want %q", got, SourceDefault)
		}
		if text := EffectiveText(def); !strings.Contains(text, "base_branch: <repository-default>") {
			t.Fatalf("effective text does not expose repository-default selection:\n%s", text)
		}
		assertEffectiveBaseBranch(t, def, nil, "default")
	})

	t.Run("explicit is preserved unchanged", func(t *testing.T) {
		const branch = "Release/Époque"
		def, err := Load(writeWorkflow(t, withBaseBranch(`base_branch: "`+branch+`"`)))
		if err != nil {
			t.Fatal(err)
		}
		if def.Config.Workspace.BaseBranch != branch {
			t.Fatalf("BaseBranch = %q, want %q", def.Config.Workspace.BaseBranch, branch)
		}
		if got := def.Provenance["workspace.base_branch"].Source; got != SourceFile {
			t.Fatalf("provenance = %q, want %q", got, SourceFile)
		}
		if text := EffectiveText(def); !strings.Contains(text, "base_branch: "+branch) {
			t.Fatalf("effective text changed the branch:\n%s", text)
		}
		assertEffectiveBaseBranch(t, def, branch, "file")
	})
}

func assertEffectiveBaseBranch(t *testing.T, def *WorkflowDefinition, want any, source string) {
	t.Helper()
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config struct {
			Workspace map[string]any `json:"workspace"`
		} `json:"config"`
		Provenance map[string]FieldOrigin `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Config.Workspace["base_branch"]; got != want {
		t.Fatalf("effective JSON base_branch = %#v, want %#v", got, want)
	}
	if got := doc.Provenance["workspace.base_branch"].Source; string(got) != source {
		t.Fatalf("effective JSON provenance = %q, want %q", got, source)
	}
}

func TestWorkspaceBaseBranchStrictFieldShape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fragment string
		field    string
	}{
		{name: "case-sensitive unknown key", fragment: "Base_Branch: main", field: "Base_Branch"},
		{name: "duplicate", fragment: "base_branch: main\n  base_branch: release", field: "base_branch"},
		{name: "explicit null", fragment: "base_branch: null", field: "workspace.base_branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeWorkflow(t, withBaseBranch(tc.fragment)))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("Load = %v, want refusal naming %q", err, tc.field)
			}
		})
	}
}

func TestWorkspaceBaseBranchGrammar(t *testing.T) {
	long := strings.Repeat("a", 256)
	max := strings.Repeat("a", 255)
	invalidUTF8 := string([]byte{'m', 'a', 'i', 'n', 0xff})
	for _, tc := range []struct {
		name   string
		branch string
		valid  bool
	}{
		{name: "one component", branch: "main", valid: true},
		{name: "slashes", branch: "release/v2", valid: true},
		{name: "similar namespace", branch: "benchmark/v2", valid: true},
		{name: "unicode and case preserved", branch: "Release/Époque", valid: true},
		{name: "shell metacharacters are git-valid", branch: `release/$USER-$(id)-"quoted"-'single'`, valid: true},
		{name: "255 byte maximum", branch: max, valid: true},
		{name: "empty", branch: ""},
		{name: "too many bytes", branch: long},
		{name: "invalid UTF-8", branch: invalidUTF8},
		{name: "leading dash", branch: "-release"},
		{name: "qualified ref", branch: "refs/heads/main"},
		{name: "remote tracking ref", branch: "origin/main"},
		{name: "issue branch namespace root", branch: "ben"},
		{name: "issue branch namespace child", branch: "ben/7"},
		{name: "dot component", branch: "release/.hidden"},
		{name: "lock suffix", branch: "release/main.lock"},
		{name: "double dot", branch: "release..next"},
		{name: "reflog syntax", branch: "main@{1}"},
		{name: "space", branch: "release candidate"},
		{name: "colon", branch: "release:v2"},
		{name: "trailing dot", branch: "release."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBaseBranch(tc.branch)
			if tc.valid && err != nil {
				t.Fatalf("validateBaseBranch(%q): %v", tc.branch, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("validateBaseBranch(%q) accepted an invalid branch", tc.branch)
			}
		})
	}
}

func TestMalformedWorkspaceBaseBranchNamesItsField(t *testing.T) {
	_, err := Load(writeWorkflow(t, withBaseBranch(`base_branch: "release candidate"`)))
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Field != "workspace.base_branch" {
		t.Fatalf("Load error = %v, want workspace.base_branch ValidationError", err)
	}
}
