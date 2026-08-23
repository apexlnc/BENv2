package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

const secretValue = "hunter2-super-secret"

func loadWithSecret(t *testing.T) *WorkflowDefinition {
	t.Helper()
	t.Setenv("GH_TEST_TOKEN", secretValue)
	content := strings.Replace(validMinimal,
		"    repo: acme/widgets",
		"    repo: acme/widgets\n    token: $GH_TEST_TOKEN", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestEffectiveTextRedactsAndAnnotates(t *testing.T) {
	def := loadWithSecret(t)
	out := EffectiveText(def)

	if strings.Contains(out, secretValue) {
		t.Fatal("effective output leaked a resolved secret")
	}
	for _, want := range []string{
		Redacted,
		"env $GH_TEST_TOKEN",
		"(file)",
		"(default)",
		"(adapter default)",
		"workflow_key: ",
		"kind: github",
		"repo: acme/widgets",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("effective text missing %q\n---\n%s", want, out)
		}
	}
}

func TestEffectiveJSONRedactsAndCarriesProvenance(t *testing.T) {
	def := loadWithSecret(t)
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secretValue) {
		t.Fatal("effective JSON leaked a resolved secret")
	}

	var doc struct {
		WorkflowKey string `json:"workflow_key"`
		Config      struct {
			Tracker struct {
				Provider map[string]any `json:"provider"`
			} `json:"tracker"`
		} `json:"config"`
		Provenance map[string]struct {
			Source  string   `json:"source"`
			EnvVars []string `json:"env_vars"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Config.Tracker.Provider["token"] != Redacted {
		t.Errorf("token = %v, want %q", doc.Config.Tracker.Provider["token"], Redacted)
	}
	if doc.Config.Tracker.Provider["repo"] != "acme/widgets" {
		t.Error("file-provided provider values should not be redacted")
	}
	p := doc.Provenance["tracker.provider.token"]
	if p.Source != "env" || !slices.Equal(p.EnvVars, []string{"GH_TEST_TOKEN"}) {
		t.Errorf("token provenance = %+v", p)
	}
	if doc.Provenance["limits.max_turns"].Source != "default" {
		t.Error("default provenance missing in JSON")
	}
}

// Structural refusals carry their offending value as data
// (core.ConfigValueError) and rendering decides by provenance (SPEC §5.8).
// Textual scrubbing failed review twice — %q-escaping and short-value
// substring collisions both defeat it — so these regressions pin the
// structured path.
func TestRenderRefusalRedactsEnvResolvedValues(t *testing.T) {
	// Quotes, backslashes, and newlines make the %q-serialized form diverge
	// from the raw value, which is what broke scrub-by-replacement.
	secret := `supersecret\line` + "\nsecond-line"
	t.Setenv("NASTY_TEST_SECRET", secret)
	content := strings.Replace(validMinimal, "repo: acme/widgets", "repo: $NASTY_TEST_SECRET", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}

	refusal := &core.ConfigValueError{
		Field: "tracker.provider.repo",
		Value: secret,
		Err:   errors.New("tracker.provider.repo must be owner/name"),
	}
	got := RenderRefusal(def, refusal)
	for _, fragment := range []string{"supersecret", "second-line"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("rendered refusal leaked %q: %q", fragment, got)
		}
	}
	if !strings.Contains(got, "[redacted $NASTY_TEST_SECRET]") {
		t.Errorf("marker should name the variable: %q", got)
	}
}

// A short env secret must leave the surrounding message intact — the old
// textual scrubbing replaced every matching character of the refusal text.
func TestRenderRefusalShortSecretLeavesMessageIntact(t *testing.T) {
	t.Setenv("SHORT_TEST_SECRET", "o") // 'o' occurs throughout the refusal text
	content := strings.Replace(validMinimal, "repo: acme/widgets", "repo: $SHORT_TEST_SECRET", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}

	refusal := &core.ConfigValueError{
		Field: "tracker.provider.repo",
		Value: "o",
		Err:   errors.New("tracker.provider.repo must be owner/name"),
	}
	want := "tracker.provider.repo must be owner/name: got [redacted $SHORT_TEST_SECRET]"
	if got := RenderRefusal(def, refusal); got != want {
		t.Errorf("RenderRefusal = %q, want %q", got, want)
	}
}

func TestRenderRefusalShowsFileValuesAndFailsClosed(t *testing.T) {
	def, err := Load(writeWorkflow(t, validMinimal))
	if err != nil {
		t.Fatal(err)
	}
	named := errors.New("tracker.provider.repo must be owner/name")

	// A file-sourced value is already public to anyone who can read the
	// repo, and showing it is what makes the refusal diagnosable.
	fileRefusal := &core.ConfigValueError{Field: "tracker.provider.repo", Value: "acme/widgets", Err: named}
	if got := RenderRefusal(def, fileRefusal); !strings.Contains(got, `got "acme/widgets"`) {
		t.Errorf("file-sourced value hidden: %q", got)
	}

	// Unknown provenance cannot prove the value is public: fail closed.
	unknown := &core.ConfigValueError{Field: "tracker.provider.mystery", Value: "who-knows", Err: named}
	if got := RenderRefusal(def, unknown); strings.Contains(got, "who-knows") || !strings.Contains(got, Redacted) {
		t.Errorf("unknown provenance must redact: %q", got)
	}

	// Refusals without a carried value pass through untouched.
	if got := RenderRefusal(def, named); got != named.Error() {
		t.Errorf("plain error rewritten: %q", got)
	}
}

// File provenance proves a value is in the repo, and nothing more. Two other
// things make one a secret, and neither is answerable from where it came:
// the *path* can be one an adapter declares sensitive, and the *value* can carry
// a credential in its shape — a URL with userinfo, which is why this renderer
// cannot decide on provenance alone (#52; PR #115 review, P1).
func TestRenderRefusalRedactsFileValuesTheProducerOrTheAdapterCallsSecret(t *testing.T) {
	content := strings.Replace(validMinimal, "repo: acme/widgets",
		"repo: acme/widgets\n    token: hunter2\n    api_url: https://ghe.example.com/", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}
	if origin, ok := def.Provenance["tracker.provider.token"]; !ok || origin.Source == SourceEnv {
		t.Fatalf("fixture token provenance = %+v; this test needs a file-sourced value to prove anything", origin)
	}
	named := errors.New("tracker.provider.api_url must name only an endpoint")

	// The path the tracker kind declares sensitive, written as a literal.
	declared := &core.ConfigValueError{
		Field: "tracker.provider.token",
		Value: "hunter2",
		Err:   errors.New("tracker.provider.token unusable"),
	}
	if got := RenderRefusal(def, declared); strings.Contains(got, "hunter2") || !strings.Contains(got, Redacted) {
		t.Errorf("a refusal at an adapter-declared sensitive path printed it: %q", got)
	}

	// A path that is deliberately *not* sensitive — an address operators need to
	// read — holding a value the producer knows is a credential.
	marked := &core.ConfigValueError{
		Field:     "tracker.provider.api_url",
		Value:     "https://ben:hunter2@ghe.example.com/",
		Sensitive: true,
		Err:       named,
	}
	if got := RenderRefusal(def, marked); strings.Contains(got, "hunter2") || !strings.Contains(got, Redacted) {
		t.Errorf("a refusal the producer marked sensitive printed it: %q", got)
	}

	// And the anchor: the same field with the same provenance, unmarked, still
	// shows its value. Without it, redacting everything would pass.
	plain := &core.ConfigValueError{Field: "tracker.provider.api_url", Value: "https://ghe.example.com/", Err: named}
	if got := RenderRefusal(def, plain); !strings.Contains(got, `got "https://ghe.example.com/"`) {
		t.Errorf("an address with nothing secret in it was hidden: %q", got)
	}
}

func TestEffectiveRedactsNestedListAndWorkspaceSecrets(t *testing.T) {
	listSecret := secretValue + "-list"
	rootSecret := filepath.Join(t.TempDir(), secretValue+"-root")
	t.Setenv("LIST_TEST_SECRET", listSecret)
	t.Setenv("ROOT_TEST_SECRET", rootSecret)

	content := strings.Replace(validMinimal,
		"    repo: acme/widgets",
		"    repo: acme/widgets\n    credentials:\n      - $LIST_TEST_SECRET\n      - literal", 1)
	content = strings.Replace(content,
		"agent:",
		"workspace:\n  root: $ROOT_TEST_SECRET\nagent:", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}

	text := EffectiveText(def)
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{text, string(raw)} {
		if strings.Contains(output, listSecret) || strings.Contains(output, rootSecret) {
			t.Fatalf("effective output leaked a resolved secret:\n%s", output)
		}
	}

	var doc struct {
		Config struct {
			Tracker struct {
				Provider map[string]any `json:"provider"`
			} `json:"tracker"`
			Workspace map[string]any `json:"workspace"`
		} `json:"config"`
		Provenance map[string]struct {
			Source  string   `json:"source"`
			EnvVars []string `json:"env_vars"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	credentials, ok := doc.Config.Tracker.Provider["credentials"].([]any)
	if !ok || len(credentials) != 2 {
		t.Fatalf("credentials = %#v, want two-element list", doc.Config.Tracker.Provider["credentials"])
	}
	if credentials[0] != Redacted || credentials[1] != "literal" {
		t.Errorf("credentials = %#v, want [%q literal]", credentials, Redacted)
	}
	if doc.Config.Workspace["root"] != Redacted {
		t.Errorf("workspace.root = %v, want %q", doc.Config.Workspace["root"], Redacted)
	}
	if got := doc.Provenance["tracker.provider.credentials[0]"]; got.Source != "env" || !slices.Equal(got.EnvVars, []string{"LIST_TEST_SECRET"}) {
		t.Errorf("list secret provenance = %+v", got)
	}
}

func TestEffectiveRedactionPathsDoNotCollide(t *testing.T) {
	listSecret := secretValue + "-collision-list"
	dotSecret := secretValue + "-collision-dot"
	t.Setenv("COLLIDING_LIST_SECRET", listSecret)
	t.Setenv("COLLIDING_DOT_SECRET", dotSecret)

	content := strings.Replace(validMinimal,
		"    repo: acme/widgets",
		`    repo: acme/widgets
    cred:
      - $COLLIDING_LIST_SECRET
    "cred[0]": literal-list
    nested:
      token: $COLLIDING_DOT_SECRET
    "nested.token": literal-dot`, 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatal(err)
	}

	wantOrigins := map[string]FieldOrigin{
		"tracker.provider.cred[0]":         {Source: SourceEnv, EnvVars: []string{"COLLIDING_LIST_SECRET"}},
		`tracker.provider["cred[0]"]`:      {Source: SourceFile},
		"tracker.provider.nested.token":    {Source: SourceEnv, EnvVars: []string{"COLLIDING_DOT_SECRET"}},
		`tracker.provider["nested.token"]`: {Source: SourceFile},
	}
	for path, want := range wantOrigins {
		got := def.Provenance[path]
		if got.Source != want.Source || !slices.Equal(got.EnvVars, want.EnvVars) {
			t.Errorf("provenance[%q] = %+v, want %+v", path, got, want)
		}
	}

	text := EffectiveText(def)
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{text, string(raw)} {
		if strings.Contains(output, listSecret) || strings.Contains(output, dotSecret) {
			t.Fatalf("effective output leaked a resolved secret:\n%s", output)
		}
	}

	var doc struct {
		Config struct {
			Tracker struct {
				Provider map[string]any `json:"provider"`
			} `json:"tracker"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	provider := doc.Config.Tracker.Provider
	cred, ok := provider["cred"].([]any)
	if !ok || len(cred) != 1 || cred[0] != Redacted {
		t.Errorf("cred = %#v, want [%q]", provider["cred"], Redacted)
	}
	if provider["cred[0]"] != "literal-list" || provider["nested.token"] != "literal-dot" {
		t.Errorf("literal collision keys changed: %#v", provider)
	}
	nested, ok := provider["nested"].(map[string]any)
	if !ok || nested["token"] != Redacted {
		t.Errorf("nested = %#v, want redacted token", provider["nested"])
	}
}

// Every knob the loader accepts is visible in `config effective` — text, JSON,
// and provenance. A knob added to the schema and forgotten in the renderer is
// configuration an operator cannot inspect, which is how `max_prompt_bytes`
// would have landed (#50). Driven off rawLimits' yaml tags, so it covers the
// knobs added after this test as well as the ones present today.
func TestEffectiveOutputCoversEveryLimitsKnob(t *testing.T) {
	def := loadWithSecret(t)
	text := EffectiveText(def)
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config struct {
			Limits map[string]any `json:"limits"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	rt := reflect.TypeOf(rawLimits{})
	for i := range rt.NumField() {
		key := rt.Field(i).Tag.Get("yaml")
		if key == "" {
			t.Fatalf("rawLimits.%s has no yaml tag", rt.Field(i).Name)
		}
		if _, ok := doc.Config.Limits[key]; !ok {
			t.Errorf("EffectiveJSON omits limits.%s", key)
		}
		if !strings.Contains(text, key+":") {
			t.Errorf("EffectiveText omits limits.%s", key)
		}
		if _, ok := def.Provenance["limits."+key]; !ok {
			t.Errorf("no provenance recorded for limits.%s", key)
		}
	}
}
