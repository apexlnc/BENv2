package config

import (
	"strings"
)

// splitFrontMatter separates the YAML front matter from the prompt body
// (SPEC §5.1). The file must begin with a `---` line; the front matter runs
// until the next `---` line; everything after is the body, trimmed. bodyLine
// is the 1-based file line the trimmed body starts on, so template errors
// can point at real WORKFLOW.md lines.
func splitFrontMatter(content string) (frontMatter, body string, bodyLine int, err error) {
	// Normalize CRLF so delimiter detection is line-based either way.
	content = strings.ReplaceAll(content, "\r\n", "\n")

	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return "", "", 0, ErrMissingFrontMatter
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			frontMatter = strings.Join(lines[1:i], "\n")
			body = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			bodyLine = i + 2
			for _, l := range lines[i+1:] {
				if strings.TrimSpace(l) != "" {
					break
				}
				bodyLine++
			}
			return frontMatter, body, bodyLine, nil
		}
	}
	return "", "", 0, ErrMissingFrontMatter
}
