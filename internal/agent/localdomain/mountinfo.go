package localdomain

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

type mountRecord struct {
	ID           uint64
	Parent       uint64
	DevMajor     uint32
	DevMinor     uint32
	Root         string
	Target       string
	Options      []string
	Optional     []string
	Filesystem   string
	Source       string
	SuperOptions []string
}

func parseMountInfo(r io.Reader) ([]mountRecord, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var records []mountRecord
	line := 0
	for scanner.Scan() {
		line++
		record, err := parseMountLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d: %w", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("mountinfo is empty")
	}
	return records, nil
}

func parseMountLine(line string) (mountRecord, error) {
	fields := strings.Fields(line)
	separator := -1
	for i, field := range fields {
		if field == "-" {
			if separator != -1 {
				return mountRecord{}, fmt.Errorf("multiple separators")
			}
			separator = i
		}
	}
	if separator < 6 || len(fields)-separator != 4 {
		return mountRecord{}, fmt.Errorf("invalid field shape")
	}
	id, err := parsePositiveDecimal(fields[0], 64)
	if err != nil {
		return mountRecord{}, fmt.Errorf("mount id: %w", err)
	}
	parent, err := parsePositiveDecimal(fields[1], 64)
	if err != nil {
		return mountRecord{}, fmt.Errorf("parent id: %w", err)
	}
	dev, err := parseMountDevice(fields[2])
	if err != nil {
		return mountRecord{}, err
	}
	root, err := unescapeMountField(fields[3])
	if err != nil {
		return mountRecord{}, fmt.Errorf("root: %w", err)
	}
	target, err := unescapeMountField(fields[4])
	if err != nil {
		return mountRecord{}, fmt.Errorf("target: %w", err)
	}
	if !validMountRoot(root) {
		return mountRecord{}, fmt.Errorf("noncanonical root %q", root)
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return mountRecord{}, fmt.Errorf("noncanonical target %q", target)
	}
	source, err := unescapeMountField(fields[separator+2])
	if err != nil {
		return mountRecord{}, fmt.Errorf("source: %w", err)
	}
	return mountRecord{
		ID:           id,
		Parent:       parent,
		DevMajor:     dev[0],
		DevMinor:     dev[1],
		Root:         root,
		Target:       target,
		Options:      splitMountOptions(fields[5]),
		Optional:     append([]string(nil), fields[6:separator]...),
		Filesystem:   fields[separator+1],
		Source:       source,
		SuperOptions: splitMountOptions(fields[separator+3]),
	}, nil
}

// A cgroup mount inherited across a cgroup-namespace boundary can have a root
// such as /../../..: the kernel is describing an ancestor outside the current
// view, not a path userspace may resolve. Keep root metadata strict without
// applying filepath.Clean, which would erase that security-relevant shape.
func validMountRoot(root string) bool {
	if root == "/" {
		return true
	}
	if !strings.HasPrefix(root, "/") || strings.HasSuffix(root, "/") {
		return false
	}
	for _, element := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if element == "" || element == "." {
			return false
		}
	}
	return true
}

func parsePositiveDecimal(value string, bits int) (uint64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty decimal")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("nondecimal %q", value)
		}
	}
	parsed, err := strconv.ParseUint(value, 10, bits)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("zero or overflow %q", value)
	}
	return parsed, nil
}

func parseMountDevice(value string) ([2]uint32, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return [2]uint32{}, fmt.Errorf("invalid device %q", value)
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return [2]uint32{}, fmt.Errorf("invalid device major %q", parts[0])
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return [2]uint32{}, fmt.Errorf("invalid device minor %q", parts[1])
	}
	return [2]uint32{uint32(major), uint32(minor)}, nil
}

func unescapeMountField(value string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			result.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("short escape")
		}
		escaped := value[i+1 : i+4]
		switch escaped {
		case "040":
			result.WriteByte(' ')
		case "011":
			result.WriteByte('\t')
		case "012":
			result.WriteByte('\n')
		case "134":
			result.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported escape \\%s", escaped)
		}
		i += 3
	}
	return result.String(), nil
}

func splitMountOptions(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func hasMountOption(record mountRecord, option string) bool {
	for _, candidate := range record.Options {
		if candidate == option {
			return true
		}
	}
	for _, candidate := range record.SuperOptions {
		if candidate == option {
			return true
		}
	}
	return false
}

func isSharedMount(record mountRecord) bool {
	for _, field := range record.Optional {
		if strings.HasPrefix(field, "shared:") {
			return true
		}
	}
	return false
}

func hasMountPropagation(record mountRecord) bool {
	for _, field := range record.Optional {
		if strings.HasPrefix(field, "shared:") || strings.HasPrefix(field, "master:") ||
			strings.HasPrefix(field, "propagate_from:") {
			return true
		}
	}
	return false
}
