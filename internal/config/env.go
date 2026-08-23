package config

import (
	"os"
	"regexp"
	"slices"
	"strings"
)

// $VAR_NAME references: uppercase, digits, underscore (SPEC §5.5).
var envVarRe = regexp.MustCompile(`\$([A-Z_][A-Z0-9_]*)`)

// resolveEnvString applies explicit $VAR indirection to one string value: a
// value opts in by containing a reference. Every referenced variable must be
// set and non-empty (empty resolution = missing secret). Returns the resolved
// value and **every** variable it interpolated, in order, nil if it contained
// none.
//
// Every one, not the last one. An interpolated value carries a secret from each
// variable it names, so a provenance entry recording only one of them describes
// the value inaccurately — and the §10.2 credential check reads exactly this to
// decide whether a secret is shared. `$TRACKER_PAT-$SUFFIX` recorded as
// "SUFFIX" is a tracker credential the check cannot see.
func resolveEnvString(value, fieldPath string) (resolved string, envVars []string, err error) {
	matches := envVarRe.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		name := value[m[2]:m[3]]
		v := os.Getenv(name)
		if v == "" {
			return "", nil, &MissingSecretError{Var: name, Field: fieldPath}
		}
		b.WriteString(value[last:m[0]])
		b.WriteString(v)
		last = m[1]
		if !slices.Contains(envVars, name) {
			envVars = append(envVars, name)
		}
	}
	b.WriteString(value[last:])
	return b.String(), envVars, nil
}

// resolveProviderEnv walks an adapter-owned provider block and resolves $VAR
// references in every string leaf (including inside nested maps and lists),
// recording provenance per dotted path. Env-resolved provider values are
// treated as secrets by `config effective` rendering (SPEC §5.8).
//
// The block is mutated in place; non-string leaves pass through untouched.
func resolveProviderEnv(block map[string]any, pathPrefix string, prov Provenance) error {
	for k, v := range block {
		path := appendProvenanceMapKey(pathPrefix, k)
		resolved, err := resolveEnvValue(v, path, prov)
		if err != nil {
			return err
		}
		block[k] = resolved
	}
	return nil
}

func resolveEnvValue(v any, path string, prov Provenance) (any, error) {
	switch val := v.(type) {
	case string:
		resolved, envVars, err := resolveEnvString(val, path)
		if err != nil {
			return nil, err
		}
		if len(envVars) > 0 {
			prov[path] = FieldOrigin{Source: SourceEnv, EnvVars: envVars}
		} else {
			prov[path] = FieldOrigin{Source: SourceFile}
		}
		return resolved, nil
	case map[string]any:
		if err := resolveProviderEnv(val, path, prov); err != nil {
			return nil, err
		}
		return val, nil
	case []any:
		// The collection itself came from the file; each element gets its own
		// indexed provenance path so a literal sibling cannot overwrite an
		// env-resolved secret's redaction marker.
		prov[path] = FieldOrigin{Source: SourceFile}
		for i, item := range val {
			resolved, err := resolveEnvValue(item, appendProvenanceIndex(path, i), prov)
			if err != nil {
				return nil, err
			}
			val[i] = resolved
		}
		return val, nil
	default:
		prov[path] = FieldOrigin{Source: SourceFile}
		return v, nil
	}
}
