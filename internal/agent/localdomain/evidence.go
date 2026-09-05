package localdomain

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// EncodeEvidence returns the one canonical byte encoding for id.
func EncodeEvidence(id Identity) (Evidence, error) {
	if err := validateIdentity(id); err != nil {
		return Evidence{}, err
	}
	payload := strings.Join([]string{
		"v1",
		"delegate=" + formatObjectID(id.Delegate),
		"root=" + formatObjectID(id.Root),
		"name=" + id.Name,
		"leaf=" + formatObjectID(id.Leaf),
		"pid=" + strconv.FormatUint(uint64(id.PID), 10),
		"start=" + strconv.FormatUint(id.StartTicks, 10),
		"pidns=" + formatObjectID(id.PIDNS),
		"cgns=" + formatObjectID(id.CgroupNS),
	}, ";")
	return Evidence{Scheme: EvidenceScheme, Boot: id.Boot, ID: payload}, nil
}

// ParseEvidence rejects every representation other than EncodeEvidence's exact
// output, including unknown versions and reordered, duplicated, or extra fields.
func ParseEvidence(e Evidence) (Identity, error) {
	if e.Scheme != EvidenceScheme {
		return Identity{}, invalidEvidence("scheme %q", e.Scheme)
	}
	if !validBootID(e.Boot) {
		return Identity{}, invalidEvidence("boot %q", e.Boot)
	}
	parts := strings.Split(e.ID, ";")
	if len(parts) != 9 || parts[0] != "v1" {
		return Identity{}, invalidEvidence("payload shape")
	}
	want := [...]string{"delegate", "root", "name", "leaf", "pid", "start", "pidns", "cgns"}
	values := make([]string, len(want))
	for i, key := range want {
		prefix := key + "="
		if !strings.HasPrefix(parts[i+1], prefix) {
			return Identity{}, invalidEvidence("field %d is not %s", i+1, key)
		}
		values[i] = strings.TrimPrefix(parts[i+1], prefix)
		if values[i] == "" {
			return Identity{}, invalidEvidence("empty %s", key)
		}
	}
	delegate, err := parseObjectID(values[0])
	if err != nil {
		return Identity{}, invalidEvidence("delegate: %v", err)
	}
	root, err := parseObjectID(values[1])
	if err != nil {
		return Identity{}, invalidEvidence("root: %v", err)
	}
	if !validNonce(values[2]) {
		return Identity{}, invalidEvidence("name %q", values[2])
	}
	leaf, err := parseObjectID(values[3])
	if err != nil {
		return Identity{}, invalidEvidence("leaf: %v", err)
	}
	pid, err := parseCanonicalUint(values[4], 32, false)
	if err != nil || pid > math.MaxInt32 {
		return Identity{}, invalidEvidence("pid %q", values[4])
	}
	start, err := parseCanonicalUint(values[5], 64, true)
	if err != nil {
		return Identity{}, invalidEvidence("start %q", values[5])
	}
	pidns, err := parseObjectID(values[6])
	if err != nil {
		return Identity{}, invalidEvidence("pidns: %v", err)
	}
	cgns, err := parseObjectID(values[7])
	if err != nil {
		return Identity{}, invalidEvidence("cgns: %v", err)
	}
	id := Identity{
		Boot:       e.Boot,
		Delegate:   delegate,
		Root:       root,
		Name:       values[2],
		Leaf:       leaf,
		PID:        uint32(pid),
		StartTicks: start,
		PIDNS:      pidns,
		CgroupNS:   cgns,
	}
	if err := validateIdentity(id); err != nil {
		return Identity{}, err
	}
	canonical, err := EncodeEvidence(id)
	if err != nil || canonical != e {
		return Identity{}, invalidEvidence("noncanonical payload")
	}
	return id, nil
}

func validateIdentity(id Identity) error {
	if !validBootID(id.Boot) {
		return invalidEvidence("boot %q", id.Boot)
	}
	for name, object := range map[string]ObjectID{
		"delegate": id.Delegate,
		"root":     id.Root,
		"leaf":     id.Leaf,
		"pidns":    id.PIDNS,
		"cgns":     id.CgroupNS,
	} {
		if object.Inode == 0 {
			return invalidEvidence("%s inode is zero", name)
		}
	}
	if !validNonce(id.Name) {
		return invalidEvidence("name %q", id.Name)
	}
	if id.PID == 0 || uint64(id.PID) > math.MaxInt32 {
		return invalidEvidence("pid %d", id.PID)
	}
	return nil
}

func formatObjectID(id ObjectID) string {
	return fmt.Sprintf("%08x:%08x:%016x", id.DevMajor, id.DevMinor, id.Inode)
}

func parseObjectID(value string) (ObjectID, error) {
	if len(value) != 34 || value[8] != ':' || value[17] != ':' || !lowerHex(value[:8]) ||
		!lowerHex(value[9:17]) || !lowerHex(value[18:]) {
		return ObjectID{}, errorsText("not canonical statx identity")
	}
	major, err := strconv.ParseUint(value[:8], 16, 32)
	if err != nil {
		return ObjectID{}, err
	}
	minor, err := strconv.ParseUint(value[9:17], 16, 32)
	if err != nil {
		return ObjectID{}, err
	}
	inode, err := strconv.ParseUint(value[18:], 16, 64)
	if err != nil || inode == 0 {
		return ObjectID{}, errorsText("zero or invalid inode")
	}
	return ObjectID{DevMajor: uint32(major), DevMinor: uint32(minor), Inode: inode}, nil
}

func parseCanonicalUint(value string, bits int, zeroOK bool) (uint64, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errorsText("noncanonical decimal")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errorsText("nondecimal value")
		}
	}
	parsed, err := strconv.ParseUint(value, 10, bits)
	if err != nil || (!zeroOK && parsed == 0) {
		return 0, errorsText("zero or overflow")
	}
	return parsed, nil
}

func validBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isLowerHex(c) {
				return false
			}
		}
	}
	return true
}

func validNonce(value string) bool { return len(value) == 32 && lowerHex(value) }

func lowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if !isLowerHex(c) {
			return false
		}
	}
	return true
}

func isLowerHex(c rune) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

func invalidEvidence(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvidence, fmt.Sprintf(format, args...))
}

type errorsText string

func (e errorsText) Error() string { return string(e) }
