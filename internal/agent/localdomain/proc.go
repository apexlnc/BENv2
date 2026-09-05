package localdomain

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func parseUnifiedCgroup(value string) (string, error) {
	lines := strings.Split(strings.TrimSuffix(value, "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::") {
		return "", fmt.Errorf("not one unified-v2 membership")
	}
	path := strings.TrimPrefix(lines[0], "0::")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("noncanonical cgroup path %q", path)
	}
	return path, nil
}

func parseProcStat(value string) (state byte, startTicks uint64, err error) {
	end := strings.LastIndex(value, ") ")
	if end < 1 {
		return 0, 0, fmt.Errorf("missing comm delimiter")
	}
	firstSpace := strings.IndexByte(value, ' ')
	if firstSpace < 1 {
		return 0, 0, fmt.Errorf("missing pid delimiter")
	}
	if _, err := parsePositiveDecimal(value[:firstSpace], 32); err != nil {
		return 0, 0, fmt.Errorf("pid: %w", err)
	}
	fields := strings.Fields(value[end+2:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("short stat record")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("starttime: %w", err)
	}
	return fields[0][0], start, nil
}
