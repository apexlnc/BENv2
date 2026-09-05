package config

import (
	"strings"
)

// frontMatterFirstLine is the 1-based file line the front matter's first line
// sits on. splitFrontMatter requires `---` at line 1 and hands on everything
// after it, so a line inside the front matter is one short of the same line in
// the file — and a refusal that quotes the shortfall points an operator's editor
// at the key above the one it means. bodyLine below does this job for the other
// half of the document; this constant does it for the front matter, whose offset
// is fixed by the `---` rule rather than by where the delimiter turned up.
const frontMatterFirstLine = 2

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
