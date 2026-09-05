package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	// SchemaVersion is the only document version #220's first replay slice
	// accepts. A number in the document, rather than an ambient decoder version,
	// makes an old fixture's meaning explicit when the format grows.
	SchemaVersion = 1
	maxAttempts   = 2
	maxSteps      = 3
)

// Outcome is the closed set of agent boundary outcomes the v0 corpus needs.
// It deliberately names one retryable failure rather than duplicating all of
// core.FailureReason before the scenario lab has a fixture for them.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeCrashed   Outcome = "crashed"
	OutcomeRunning   Outcome = "running"
)

// Publication is the closed set of publish-evidence worlds in the v0 corpus.
type Publication string

const (
	PublicationComplete  Publication = "complete"
	PublicationRewritten Publication = "rewritten"
)

// Action is a normalized event the integration driver can supply.
type Action string

const (
	ActionStart   Action = "start"
	ActionAdvance Action = "advance"
	ActionRestart Action = "restart"
)

// State is a stable observation boundary supported by the v0 driver. These
// spellings are BEN's production state names, but this package does not decide
// transitions between them.
type State string

const (
	StateRunning     State = "running"
	StateBackoff     State = "backoff"
	StateDone        State = "done"
	StateNeedsReview State = "needs-review"
)

// PriorRun is the positive run-absence fact a restart action may supply.
type PriorRun string

const PriorRunGone PriorRun = "gone"

// Document is one deterministic scenario. Its fields select only normalized
// boundary inputs; expected transitions and writes live in reviewed golden
// traces and independently anchored integration assertions.
type Document struct {
	SchemaVersion int         `json:"schema_version"`
	Name          string      `json:"name"`
	Issue         Issue       `json:"issue"`
	Attempts      []Attempt   `json:"attempts"`
	Publication   Publication `json:"publication"`
	Steps         []Step      `json:"steps"`
}

type Issue struct {
	Identifier string `json:"identifier"`
}

type Attempt struct {
	Outcome Outcome `json:"outcome"`
	Session string  `json:"session"`
}

type Step struct {
	Action   Action   `json:"action"`
	Until    State    `json:"until"`
	PriorRun PriorRun `json:"prior_run,omitempty"`
}

// jsonShape is the exact wire shape accepted before encoding/json maps values
// into Go fields. DisallowUnknownFields is not enough here: encoding/json
// matches field names without regard to case and silently keeps the last
// duplicate key, either of which would make a reviewed scenario ambiguous.
type jsonShape struct {
	fields  map[string]*jsonShape
	element *jsonShape
}

var (
	scalarJSON        = &jsonShape{}
	documentJSONShape = &jsonShape{fields: map[string]*jsonShape{
		"schema_version": scalarJSON,
		"name":           scalarJSON,
		"issue": {fields: map[string]*jsonShape{
			"identifier": scalarJSON,
		}},
		"attempts": {element: &jsonShape{fields: map[string]*jsonShape{
			"outcome": scalarJSON,
			"session": scalarJSON,
		}}},
		"publication": scalarJSON,
		"steps": {element: &jsonShape{fields: map[string]*jsonShape{
			"action":    scalarJSON,
			"until":     scalarJSON,
			"prior_run": scalarJSON,
		}}},
	}}
)

// Decode strictly decodes and validates one document. The strictness is part
// of the trace's meaning: a misspelled action cannot silently become a no-op,
// and a newer document cannot be replayed under older semantics.
func Decode(r io.Reader) (Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Document{}, fmt.Errorf("scenario: read: %w", err)
	}
	if err := validateJSONShape(data); err != nil {
		return Document{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("scenario: decode: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func validateJSONShape(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(dec, documentJSONShape, "scenario"); err != nil {
		return fmt.Errorf("scenario: decode: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("scenario: trailing JSON value")
		}
		return fmt.Errorf("scenario: trailing data: %w", err)
	}
	return nil
}

func consumeJSONValue(dec *json.Decoder, shape *jsonShape, path string) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		if shape.fields != nil || shape.element != nil {
			return fmt.Errorf("%s: expected %s", path, shape.kind())
		}
		return nil
	}

	switch delim {
	case '{':
		if shape.fields == nil {
			return fmt.Errorf("%s: expected %s, got object", path, shape.kind())
		}
		seen := make(map[string]struct{}, len(shape.fields))
		for dec.More() {
			fieldToken, err := dec.Token()
			if err != nil {
				return err
			}
			field, ok := fieldToken.(string)
			if !ok {
				return fmt.Errorf("%s: object field name is not a string", path)
			}
			if _, ok := seen[field]; ok {
				return fmt.Errorf("%s: duplicate field %q", path, field)
			}
			seen[field] = struct{}{}
			fieldShape, ok := shape.fields[field]
			if !ok {
				return fmt.Errorf("%s: unknown field %q", path, field)
			}
			if err := consumeJSONValue(dec, fieldShape, path+"."+field); err != nil {
				return err
			}
		}
		return consumeJSONClose(dec, '}')
	case '[':
		if shape.element == nil {
			return fmt.Errorf("%s: expected %s, got array", path, shape.kind())
		}
		for i := 0; dec.More(); i++ {
			if err := consumeJSONValue(dec, shape.element, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return consumeJSONClose(dec, ']')
	default:
		return fmt.Errorf("%s: unexpected delimiter %q", path, delim)
	}
}

func consumeJSONClose(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != want {
		return fmt.Errorf("expected closing delimiter %q, got %q", want, token)
	}
	return nil
}

func (s *jsonShape) kind() string {
	switch {
	case s.fields != nil:
		return "object"
	case s.element != nil:
		return "array"
	default:
		return "scalar"
	}
}

// Validate checks the bounded v0 grammar independently of the integration
// driver. It does not predict what the orchestrator will decide.
func (d Document) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("scenario.schema_version: got %d, want %d", d.SchemaVersion, SchemaVersion)
	}
	if err := cleanRequired("scenario.name", d.Name); err != nil {
		return err
	}
	if err := cleanRequired("scenario.issue.identifier", d.Issue.Identifier); err != nil {
		return err
	}
	if len(d.Attempts) == 0 || len(d.Attempts) > maxAttempts {
		return fmt.Errorf("scenario.attempts: got %d, want 1..%d", len(d.Attempts), maxAttempts)
	}
	if len(d.Steps) == 0 || len(d.Steps) > maxSteps {
		return fmt.Errorf("scenario.steps: got %d, want 1..%d", len(d.Steps), maxSteps)
	}

	running := 0
	for i, attempt := range d.Attempts {
		path := fmt.Sprintf("scenario.attempts[%d]", i)
		if err := cleanRequired(path+".session", attempt.Session); err != nil {
			return err
		}
		switch attempt.Outcome {
		case OutcomeSucceeded, OutcomeCrashed:
		case OutcomeRunning:
			running++
			if i != 0 {
				return fmt.Errorf("%s.outcome: running is supported only for the first attempt in schema v1", path)
			}
		default:
			return fmt.Errorf("%s.outcome: unsupported value %q", path, attempt.Outcome)
		}
		if i < len(d.Attempts)-1 && attempt.Outcome == OutcomeSucceeded {
			return fmt.Errorf("%s.outcome: succeeded must be the final attempt", path)
		}
	}
	if d.Attempts[len(d.Attempts)-1].Outcome != OutcomeSucceeded {
		return fmt.Errorf("scenario.attempts: the final attempt must be succeeded in schema v1")
	}

	switch d.Publication {
	case PublicationComplete, PublicationRewritten:
	default:
		return fmt.Errorf("scenario.publication: unsupported value %q", d.Publication)
	}

	restarts := 0
	for i, step := range d.Steps {
		path := fmt.Sprintf("scenario.steps[%d]", i)
		if !validState(step.Until) {
			return fmt.Errorf("%s.until: unsupported value %q", path, step.Until)
		}
		switch step.Action {
		case ActionStart:
			if i != 0 {
				return fmt.Errorf("%s.action: start must be the first step", path)
			}
			if step.PriorRun != "" {
				return fmt.Errorf("%s.prior_run: valid only for restart", path)
			}
		case ActionAdvance:
			if i == 0 {
				return fmt.Errorf("%s.action: advance requires a preceding start", path)
			}
			if step.PriorRun != "" {
				return fmt.Errorf("%s.prior_run: valid only for restart", path)
			}
		case ActionRestart:
			if i == 0 {
				return fmt.Errorf("%s.action: restart requires a preceding start", path)
			}
			restarts++
			if step.PriorRun != PriorRunGone {
				return fmt.Errorf("%s.prior_run: got %q, want %q", path, step.PriorRun, PriorRunGone)
			}
		default:
			return fmt.Errorf("%s.action: unsupported value %q", path, step.Action)
		}
	}
	if d.Steps[0].Action != ActionStart {
		return fmt.Errorf("scenario.steps[0].action: got %q, want %q", d.Steps[0].Action, ActionStart)
	}
	if restarts != running {
		return fmt.Errorf("scenario.steps: got %d restart actions for %d running attempts", restarts, running)
	}

	wantFinal := StateDone
	if d.Publication == PublicationRewritten {
		wantFinal = StateNeedsReview
	}
	if got := d.Steps[len(d.Steps)-1].Until; got != wantFinal {
		return fmt.Errorf("scenario.steps[%d].until: got %q, want %q for publication %q",
			len(d.Steps)-1, got, wantFinal, d.Publication)
	}
	return d.validateSequence(wantFinal)
}

func cleanRequired(path, value string) error {
	if value == "" {
		return fmt.Errorf("%s: required", path)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s: leading or trailing whitespace is not allowed", path)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s: control characters are not allowed", path)
		}
	}
	return nil
}

// validateSequence keeps every accepted v0 document runnable without a
// wall-clock escape hatch. More attempt shapes need a driver that can advance
// through repeated backoffs; accepting them before that driver exists would
// turn a valid document into a test that only ends at its deadlock deadline.
func (d Document) validateSequence(final State) error {
	if len(d.Attempts) == 1 {
		if len(d.Steps) != 1 || !matches(d.Steps[0], ActionStart, final, "") {
			return fmt.Errorf("scenario.steps: a single succeeded attempt requires exactly start until=%s", final)
		}
		return nil
	}

	switch d.Attempts[0].Outcome {
	case OutcomeCrashed:
		if len(d.Steps) != 2 ||
			!matches(d.Steps[0], ActionStart, StateBackoff, "") ||
			!matches(d.Steps[1], ActionAdvance, final, "") {
			return fmt.Errorf("scenario.steps: crashed then succeeded requires start until=backoff, then advance until=%s", final)
		}
	case OutcomeRunning:
		if len(d.Steps) != 3 ||
			!matches(d.Steps[0], ActionStart, StateRunning, "") ||
			!matches(d.Steps[1], ActionRestart, StateBackoff, PriorRunGone) ||
			!matches(d.Steps[2], ActionAdvance, final, "") {
			return fmt.Errorf("scenario.steps: running then succeeded requires start until=running, restart prior_run=gone until=backoff, then advance until=%s", final)
		}
	default:
		return fmt.Errorf("scenario.attempts[0].outcome: unsupported two-attempt sequence %q", d.Attempts[0].Outcome)
	}
	return nil
}

func matches(step Step, action Action, until State, prior PriorRun) bool {
	return step.Action == action && step.Until == until && step.PriorRun == prior
}

func validState(state State) bool {
	switch state {
	case StateRunning, StateBackoff, StateDone, StateNeedsReview:
		return true
	default:
		return false
	}
}
