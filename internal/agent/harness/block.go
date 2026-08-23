package harness

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ValueRefusal anchors a refusal to one `agent.provider` key and carries the
// offending value as *data*, so `config effective` can redact it by provenance
// (SPEC §5.8). Provider strings are `$VAR`-resolved before an adapter sees them
// (SPEC §5.5), so any value an adapter refuses may be a resolved secret, and a
// message that embeds it has already leaked it into a CI log — the reason
// textual scrubbing was rejected for the tracker adapter (core.ConfigValueError).
//
// err must state the rule without naming the value: the renderer appends what it
// decides to show. key is relative to the block ("permission_mode"), and Field
// becomes "agent.provider.<key>", which is the loader's provenance path. A key
// that does not match one — a mis-spelled path, or a list entry written without
// ValueRefusalIndex — is redacted unconditionally rather than shown, so the
// failure mode is a lost variable name, never a leaked value.
func ValueRefusal(key, value string, err error) error {
	return &core.ConfigValueError{Field: "agent.provider." + key, Value: value, Err: err}
}

// ValueRefusalIndex is ValueRefusal for one entry of a list-valued key. The
// bracket form mirrors the loader's indexed provenance paths
// ("agent.provider.add_dirs[0]").
func ValueRefusalIndex(key string, index int, value string, err error) error {
	return ValueRefusal(key+"["+strconv.Itoa(index)+"]", value, err)
}

// Block reads an `agent.provider` map (SPEC §5.2.5) as typed values, refusing
// anything mis-shaped with the adapter's own named error.
//
// The block is opaque to the core, which makes each adapter the only thing
// standing between a typo and a run with the wrong posture — so the key set is
// closed (Unknown) and every value is checked. Values arrive with `$VAR`
// indirection already resolved by the config loader (SPEC §5.5), so a
// secret-bearing key holds the secret itself; that is why none of them may
// reach argv (SPEC §7.6).
//
// Parsing is pure: no filesystem, no process, no network, so `ben config
// effective` can reject a malformed block on a machine that never installed the
// harness (SPEC §5.8).
type Block struct {
	m map[string]any
	// invalid is the adapter's "wrong shape or unusable value" sentinel.
	invalid error
}

// NewBlock wraps a provider map. invalid is returned (wrapped) by every value
// accessor.
func NewBlock(m map[string]any, invalid error) Block {
	return Block{m: m, invalid: invalid}
}

// Unknown refuses any key outside the adapter's closed set. A silent
// `permision_mode:` typo would otherwise run with the wrong permissions.
func (b Block) Unknown(known []string, sentinel error) error {
	names := make([]string, 0, len(b.m))
	for k := range b.m {
		names = append(names, k)
	}
	// Deterministic message when a block has several unknown keys.
	sort.Strings(names)
	for _, k := range names {
		if !slices.Contains(known, k) {
			return fmt.Errorf("%w %q (known keys: %s)", sentinel, k, strings.Join(known, ", "))
		}
	}
	return nil
}

// String reads an optional string; a missing or null key is the empty string.
func (b Block) String(key string) (string, error) {
	v, ok := b.m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a string, got %T", b.invalid, key, v)
	}
	return s, nil
}

// Bool reads an optional boolean; a missing or null key is false.
func (b Block) Bool(key string) (bool, error) {
	v, ok := b.m[key]
	if !ok || v == nil {
		return false, nil
	}
	t, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %s must be true or false, got %T", b.invalid, key, v)
	}
	return t, nil
}

// Strings reads an optional list of non-empty strings.
func (b Block) Strings(key string) ([]string, error) {
	v, ok := b.m[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a list of strings, got %T", b.invalid, key, v)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s[%d] must be a string, got %T", b.invalid, key, i, e)
		}
		if s == "" {
			return nil, fmt.Errorf("%w: %s[%d] is empty", b.invalid, key, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// StringMap reads an optional map of string values, e.g. an environment block.
func (b Block) StringMap(key string) (map[string]string, error) {
	v, ok := b.m[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a map of strings, got %T", b.invalid, key, v)
	}
	out := make(map[string]string, len(m))
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	// Deterministic error for a map with several bad entries.
	sort.Strings(names)
	for _, name := range names {
		s, ok := m[name].(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s.%s must be a string, got %T", b.invalid, key, name, m[name])
		}
		if name == "" {
			return nil, fmt.Errorf("%w: %s has an empty variable name", b.invalid, key)
		}
		out[name] = s
	}
	return out, nil
}
