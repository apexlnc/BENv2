package config

import "strconv"

// appendProvenanceMapKey encodes one provider-owned map key without losing
// structure. Ordinary keys retain the readable dotted form; keys containing
// path syntax use a quoted bracket segment, so a literal "cred[0]" cannot
// collide with key "cred" followed by list index 0.
func appendProvenanceMapKey(prefix, key string) string {
	if isPlainProvenanceSegment(key) {
		return prefix + "." + key
	}
	return prefix + "[" + strconv.Quote(key) + "]"
}

func appendProvenanceIndex(prefix string, index int) string {
	return prefix + "[" + strconv.Itoa(index) + "]"
}

func isPlainProvenanceSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}
