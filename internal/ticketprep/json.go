package ticketprep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"
)

type jsonShape struct {
	fields  map[string]jsonField
	element *jsonShape
}

type jsonField struct {
	shape    *jsonShape
	required bool
}

var scalarJSON = &jsonShape{}

func object(fields map[string]jsonField) *jsonShape { return &jsonShape{fields: fields} }
func array(element *jsonShape) *jsonShape           { return &jsonShape{element: element} }
func required(shape *jsonShape) jsonField           { return jsonField{shape: shape, required: true} }
func optional(shape *jsonShape) jsonField           { return jsonField{shape: shape} }

var (
	issueShape = object(map[string]jsonField{
		"schema_version": required(scalarJSON),
		"number":         required(scalarJSON),
		"url":            required(scalarJSON),
		"title":          required(scalarJSON),
		"body":           required(scalarJSON),
	})
	subjectShape = object(map[string]jsonField{
		"repository_identity": required(scalarJSON),
		"issue_number":        required(scalarJSON),
		"issue_url":           required(scalarJSON),
		"title":               required(scalarJSON),
		"body":                required(scalarJSON),
		"content_digest":      required(scalarJSON),
	})
	repositoryShape = object(map[string]jsonField{
		"remote":             required(scalarJSON),
		"identity":           required(scalarJSON),
		"remote_fingerprint": required(scalarJSON),
		"commit":             required(scalarJSON),
		"tree":               required(scalarJSON),
	})
	pathFactShape = object(map[string]jsonField{
		"reference":     required(scalarJSON),
		"status":        required(scalarJSON),
		"resolved_path": optional(scalarJSON),
		"blob":          optional(scalarJSON),
		"evidence":      required(scalarJSON),
		"reason":        optional(scalarJSON),
	})
	symbolFactShape = object(map[string]jsonField{
		"reference": required(scalarJSON),
		"status":    required(scalarJSON),
		"name":      required(scalarJSON),
		"path":      optional(scalarJSON),
		"line":      optional(scalarJSON),
		"blob":      optional(scalarJSON),
		"evidence":  required(scalarJSON),
		"reason":    optional(scalarJSON),
	})
	instructionShape = object(map[string]jsonField{
		"path": required(scalarJSON),
		"blob": required(scalarJSON),
	})
	commandShape = object(map[string]jsonField{
		"command": required(scalarJSON),
		"source":  required(scalarJSON),
		"line":    required(scalarJSON),
		"blob":    required(scalarJSON),
	})
	unknownShape = object(map[string]jsonField{
		"reference": required(scalarJSON),
		"reason":    required(scalarJSON),
	})
	factsShape = object(map[string]jsonField{
		"paths":               required(array(pathFactShape)),
		"symbols":             required(array(symbolFactShape)),
		"instruction_files":   required(array(instructionShape)),
		"validation_commands": required(array(commandShape)),
		"unknown":             required(array(unknownShape)),
	})
	sourcesShape = object(map[string]jsonField{
		"issue":      required(scalarJSON),
		"repository": required(scalarJSON),
		"remote":     required(scalarJSON),
	})
	captureShape = object(map[string]jsonField{
		"schema_version": required(scalarJSON),
		"kernel_version": required(scalarJSON),
		"subject":        required(subjectShape),
		"repository":     required(repositoryShape),
		"facts":          required(factsShape),
		"sources":        required(sourcesShape),
	})
	provenanceShape = object(map[string]jsonField{
		"provider": required(scalarJSON),
		"model":    required(scalarJSON),
		"command":  required(scalarJSON),
		"prompt":   required(scalarJSON),
	})
	decisionShape = object(map[string]jsonField{
		"question":           required(scalarJSON),
		"kind":               required(scalarJSON),
		"material_effect":    required(scalarJSON),
		"changes":            required(scalarJSON),
		"options":            required(array(scalarJSON)),
		"recommended_option": required(scalarJSON),
	})
	decompositionShape = object(map[string]jsonField{
		"destination":         required(scalarJSON),
		"not_yet_specifiable": required(array(scalarJSON)),
		"out_of_scope":        required(array(scalarJSON)),
	})
	splitShape = object(map[string]jsonField{
		"outcome":                     required(scalarJSON),
		"independently_verifiable_by": required(scalarJSON),
		"blocked_by":                  required(array(scalarJSON)),
	})
	adviceShape = object(map[string]jsonField{
		"restated_outcome":          required(scalarJSON),
		"candidate_non_goals":       required(array(scalarJSON)),
		"assumptions_to_confirm":    required(array(scalarJSON)),
		"decision_queue":            required(array(decisionShape)),
		"applicable_constraints":    required(array(scalarJSON)),
		"acceptance_gaps":           required(array(scalarJSON)),
		"proposed_acceptance_tests": required(array(scalarJSON)),
		"affected_area_hypotheses":  required(array(scalarJSON)),
		"decision_decomposition":    optional(decompositionShape),
		"candidate_delivery_splits": required(array(splitShape)),
		"recommendation":            required(scalarJSON),
		"reasons":                   required(array(scalarJSON)),
	})
	adviceDocumentShape = object(map[string]jsonField{
		"schema_version":      required(scalarJSON),
		"declared_provenance": required(provenanceShape),
		"advice":              required(adviceShape),
	})
	packetShape = object(map[string]jsonField{
		"schema_version":      required(scalarJSON),
		"kernel_version":      required(scalarJSON),
		"capture":             required(captureShape),
		"declared_provenance": required(provenanceShape),
		"advice":              required(adviceShape),
	})
	dispositionEntryShape = object(map[string]jsonField{
		"suggestion_id":      required(scalarJSON),
		"disposition":        required(scalarJSON),
		"selected_option_id": optional(scalarJSON),
	})
	dispositionShape = object(map[string]jsonField{
		"schema_version": required(scalarJSON),
		"packet_digest":  required(scalarJSON),
		"items":          required(array(dispositionEntryShape)),
	})
)

func DecodeIssue(r io.Reader) (IssueInput, error) {
	var issue IssueInput
	if err := decodeExact(r, maxArtifactBytes, issueShape, "issue", &issue); err != nil {
		return IssueInput{}, err
	}
	if err := issue.Validate(); err != nil {
		return IssueInput{}, err
	}
	return issue, nil
}

func DecodeCapture(r io.Reader) (Capture, error) {
	var capture Capture
	if err := decodeExact(r, maxArtifactBytes, captureShape, "capture", &capture); err != nil {
		return Capture{}, err
	}
	if err := capture.Validate(); err != nil {
		return Capture{}, err
	}
	return capture, nil
}

func DecodeAdvice(r io.Reader) (AdviceDocument, error) {
	var advice AdviceDocument
	if err := decodeExact(r, maxAdviceBytes, adviceDocumentShape, "advice_document", &advice); err != nil {
		return AdviceDocument{}, err
	}
	if err := advice.Validate(); err != nil {
		return AdviceDocument{}, err
	}
	return advice, nil
}

func DecodePacket(r io.Reader) (Packet, error) {
	var packet Packet
	if err := decodeExact(r, maxArtifactBytes, packetShape, "packet", &packet); err != nil {
		return Packet{}, err
	}
	if err := packet.Validate(); err != nil {
		return Packet{}, err
	}
	return packet, nil
}

func DecodeDispositions(r io.Reader) (DispositionDocument, error) {
	var dispositions DispositionDocument
	if err := decodeExact(r, maxAdviceBytes, dispositionShape, "dispositions", &dispositions); err != nil {
		return DispositionDocument{}, err
	}
	return dispositions, nil
}

func Encode(w io.Writer, value any) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("ticketprep: encode artifact: %w", err)
	}
	limit := maxArtifactBytes
	switch value.(type) {
	case AdviceDocument, *AdviceDocument, DispositionDocument, *DispositionDocument:
		limit = maxAdviceBytes
	}
	if body.Len() > limit {
		return fmt.Errorf("%w: encoded artifact has %d bytes, max %d", ErrArtifactTooLarge, body.Len(), limit)
	}
	if _, err := io.Copy(w, &body); err != nil {
		return fmt.Errorf("ticketprep: write artifact: %w", err)
	}
	return nil
}

func decodeExact(r io.Reader, limit int, shape *jsonShape, label string, out any) error {
	data, err := readBounded(r, limit)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return ErrInvalidUTF8
	}
	if err := validateUnicodeEscapes(data); err != nil {
		return err
	}
	if err := validateJSONShape(data, shape, label); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidJSON, label, err)
	}
	return nil
}

func readBounded(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("ticketprep: read artifact: %w", err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: got more than %d bytes", ErrArtifactTooLarge, limit)
	}
	return data, nil
}

// encoding/json replaces an unpaired UTF-16 surrogate escape with U+FFFD.
// That recovery would change the decoded bytes supplied by capture, so reject
// such escapes before decoding instead of silently assigning them a digest.
func validateUnicodeEscapes(data []byte) error {
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' || i+1 >= len(data) {
				continue
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := hexQuad(data, i+2)
			if !ok {
				continue // the JSON decoder reports malformed escape syntax
			}
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				low, paired := hexQuad(data, i+8)
				if i+7 >= len(data) || data[i+6] != '\\' || data[i+7] != 'u' || !paired || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("%w: unpaired UTF-16 surrogate escape", ErrInvalidUTF8)
				}
				i += 11
			case unit >= 0xdc00 && unit <= 0xdfff:
				return fmt.Errorf("%w: unpaired UTF-16 surrogate escape", ErrInvalidUTF8)
			default:
				i += 5
			}
		}
	}
	return nil
}

func hexQuad(data []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(data) {
		return 0, false
	}
	var value uint16
	for _, c := range data[start : start+4] {
		value <<= 4
		switch {
		case c >= '0' && c <= '9':
			value += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			value += uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			value += uint16(c-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func validateJSONShape(data []byte, shape *jsonShape, path string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(dec, shape, path); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return ErrTrailingJSON
		}
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return nil
}

func consumeJSONValue(dec *json.Decoder, shape *jsonShape, path string) error {
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidJSON, path, err)
	}
	delim, composite := token.(json.Delim)
	if !composite {
		if token == nil {
			return fmt.Errorf("%w: %s: null is not supported", ErrInvalidValue, path)
		}
		if shape.fields != nil || shape.element != nil {
			return fmt.Errorf("%w: %s: expected %s", ErrInvalidJSON, path, shape.kind())
		}
		return nil
	}

	switch delim {
	case '{':
		if shape.fields == nil {
			return fmt.Errorf("%w: %s: expected %s, got object", ErrInvalidJSON, path, shape.kind())
		}
		seen := make(map[string]bool, len(shape.fields))
		for dec.More() {
			fieldToken, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrInvalidJSON, path, err)
			}
			field, ok := fieldToken.(string)
			if !ok {
				return fmt.Errorf("%w: %s: object field name is not a string", ErrInvalidJSON, path)
			}
			if seen[field] {
				return fmt.Errorf("%w: %s.%s", ErrDuplicateField, path, field)
			}
			seen[field] = true
			fieldShape, ok := shape.fields[field]
			if !ok {
				return fmt.Errorf("%w: %s.%s", ErrUnknownField, path, field)
			}
			if err := consumeJSONValue(dec, fieldShape.shape, path+"."+field); err != nil {
				return err
			}
		}
		if err := consumeJSONClose(dec, '}'); err != nil {
			return err
		}
		var missing []string
		for name, field := range shape.fields {
			if field.required && !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			return fmt.Errorf("%w: %s: missing required fields %v", ErrInvalidValue, path, missing)
		}
		return nil
	case '[':
		if shape.element == nil {
			return fmt.Errorf("%w: %s: expected %s, got array", ErrInvalidJSON, path, shape.kind())
		}
		for i := 0; dec.More(); i++ {
			if err := consumeJSONValue(dec, shape.element, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return consumeJSONClose(dec, ']')
	default:
		return fmt.Errorf("%w: %s: unexpected delimiter %q", ErrInvalidJSON, path, delim)
	}
}

func consumeJSONClose(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if token != want {
		return fmt.Errorf("%w: expected closing delimiter %q, got %q", ErrInvalidJSON, want, token)
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

func isOneOf[T comparable](got T, values ...T) bool {
	for _, value := range values {
		if got == value {
			return true
		}
	}
	return false
}

func unsupported(path string, value any) error {
	return fmt.Errorf("%w: %s = %v", ErrUnsupported, path, value)
}

func boundedString(path, value string, max int, emptyOK bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s", ErrInvalidUTF8, path)
	}
	if len(value) > max {
		return fmt.Errorf("%w: %s has %d bytes, max %d", ErrBoundExceeded, path, len(value), max)
	}
	if !emptyOK && len(bytes.TrimSpace([]byte(value))) == 0 {
		return fmt.Errorf("%w: %s is empty", ErrInvalidValue, path)
	}
	return nil
}

func boundedList(path string, values []string, maxCount int) error {
	if len(values) > maxCount {
		return fmt.Errorf("%w: %s has %d values, max %d", ErrBoundExceeded, path, len(values), maxCount)
	}
	for i, value := range values {
		if err := boundedString(fmt.Sprintf("%s[%d]", path, i), value, maxTextBytes, false); err != nil {
			return err
		}
	}
	return nil
}

func version(path string, got int) error {
	if got != SchemaVersion {
		return unsupported(path, got)
	}
	return nil
}

func joinErrors(errs ...error) error {
	return errors.Join(errs...)
}
