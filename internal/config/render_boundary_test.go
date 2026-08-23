// An external test package on purpose: what it asserts is what a *caller* can
// reach, which is invisible from inside the package.
package config_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// The configured ceiling must not be bypassable from outside the package
// (SPEC §5.6, #50). `limits.max_prompt_bytes` is worth nothing if a call site
// can get the compiled template and supply its own limits — the shape
// b08-orchestrator had, `Prompt.Render(vars, template.Limits{})`, which reads as
// wired while enforcing the package default in place of the operator's value.
//
// RenderPrompt is the only render path, and this is what keeps it the only one:
// re-exporting the field, or adding an accessor that hands out the template or
// its limits, fails here rather than silently reopening the hole. A cross-PR
// reminder is not an enforceable invariant (PR #75 review).
func TestNoExportedPathToAnUnboundedRender(t *testing.T) {
	defType := reflect.TypeOf(config.WorkflowDefinition{})

	for i := range defType.NumField() {
		f := defType.Field(i)
		if !f.IsExported() {
			continue
		}
		if found, ok := reaches(f.Type); ok {
			t.Errorf("WorkflowDefinition.%s (%s) exposes %s — a caller holding it renders under a ceiling it chose",
				f.Name, f.Type, found)
		}
	}

	ptr := reflect.PointerTo(defType)
	for i := range ptr.NumMethod() {
		m := ptr.Method(i)
		for j := range m.Type.NumOut() {
			out := m.Type.Out(j)
			if found, ok := reaches(out); ok {
				t.Errorf("(*WorkflowDefinition).%s returns %s, exposing %s — same hole, one accessor further out",
					m.Name, out, found)
			}
		}
	}
}

// forbidden are the types whose holder chooses a ceiling, keyed by the value
// form: the compiled template, and the limits it takes.
func forbidden() map[reflect.Type]bool {
	return map[reflect.Type]bool{
		reflect.TypeOf(template.Prompt{}): true,
		reflect.TypeOf(template.Limits{}): true,
	}
}

// reaches reports whether a caller holding typ can get at a forbidden type, and
// which one.
//
// It compares *normalized* types rather than exact ones, because the spelling
// does not change what the holder can do: an accessor returning template.Prompt
// by value is as usable as one returning *template.Prompt — the returned value
// is addressable once assigned, and Render's pointer receiver binds to it — and
// the same goes for *template.Limits. An equality check against two spellings
// missed both (PR #75 review round 2). So pointers, slices, arrays and maps are
// unwrapped, and it recurses through *exported* struct fields only, since an
// unexported field is not reachable from outside the package — which is the
// whole point of the invariant.
//
// Not covered, and undetectable this way: a func-typed field, or a prompt handed
// back inside an `any`. Both would be a stranger thing to write than the field
// this test exists to stop from coming back.
func reaches(typ reflect.Type) (reflect.Type, bool) {
	return reachesVia(typ, forbidden(), map[reflect.Type]bool{})
}

func reachesVia(typ reflect.Type, bad, seen map[reflect.Type]bool) (reflect.Type, bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bad[typ] {
		return typ, true
	}
	// Recursive types (a linked list of definitions would be odd, but the walk
	// must terminate regardless).
	if seen[typ] {
		return nil, false
	}
	seen[typ] = true

	switch typ.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		if found, ok := reachesVia(typ.Elem(), bad, seen); ok {
			return found, true
		}
		if typ.Kind() == reflect.Map {
			return reachesVia(typ.Key(), bad, seen)
		}
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if found, ok := reachesVia(f.Type, bad, seen); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// A definition that did not come from Load carries no compiled prompt. Every
// other field is exported, so an outside caller can assemble one — a zero value
// in a test, most likely — and that must refuse rather than panic through a nil
// template, which would read as an engine bug instead of the miswiring it is.
func TestRenderPromptRefusesADefinitionThatDidNotComeFromLoad(t *testing.T) {
	var def config.WorkflowDefinition
	out, err := def.RenderPrompt(template.Vars{
		Issue:     core.Issue{Identifier: "7", Title: "t"},
		Attempt:   1,
		Workspace: "/w",
		Run:       template.Run{ID: "r-1"},
	})
	if !errors.Is(err, config.ErrNoCompiledPrompt) {
		t.Fatalf("RenderPrompt = %q, %v; want %v", out, err, config.ErrNoCompiledPrompt)
	}
}
