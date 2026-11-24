package skill

import (
	"regexp"
	"strings"
)

// SkillMeta represents metadata extracted from SKILL.md YAML frontmatter or headings.
type SkillMeta struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Version     string         `json:"version,omitempty"`
	Raw         map[string]any `json:"raw,omitempty"`
	Body        string         `json:"-"`
}

var (
	reFrontmatter = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)
	reHeading     = regexp.MustCompile(`(?m)^\s*#\s+(.+?)\s*$`)
	reYamlKV      = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:\s*(.*)$`)
	reYamlList    = regexp.MustCompile(`^\s*-\s+(.*)$`)
	reLeading2Sp  = regexp.MustCompile(`^[ \t]{2}`)
)

// ParseSkillFrontmatter parses lightweight YAML frontmatter from SKILL.md without external dependencies.
func ParseSkillFrontmatter(markdownText string) SkillMeta {
	meta := SkillMeta{
		Raw:  map[string]any{},
		Body: markdownText,
	}

	if m := reFrontmatter.FindStringSubmatch(markdownText); len(m) == 3 {
		yamlBlock := m[1]
		meta.Body = m[2]
		parsed := parseSimpleYAML(yamlBlock)
		meta.Raw = parsed
		if v, ok := parsed["name"].(string); ok {
			meta.Name = strings.TrimSpace(v)
		}
		if v, ok := parsed["description"].(string); ok {
			meta.Description = strings.TrimSpace(v)
		}
		if v, ok := parsed["version"].(string); ok {
			meta.Version = strings.TrimSpace(v)
		}
	}

	if meta.Name == "" {
		if hm := reHeading.FindStringSubmatch(meta.Body); len(hm) == 2 {
			meta.Name = strings.TrimSpace(hm[1])
		}
	}
	return meta
}

func parseSimpleYAML(yamlBlock string) map[string]any {
	result := make(map[string]any)
	lines := strings.Split(strings.ReplaceAll(yamlBlock, "\r\n", "\n"), "\n")

	var currentKey string
	var blockMode string // "|", ">", "list"
	var blockLines []string

	flushBlock := func() {
		if currentKey == "" {
			return
		}
		switch blockMode {
		case "list":
			cp := make([]string, len(blockLines))
			copy(cp, blockLines)
			result[currentKey] = cp
		case "|":
			result[currentKey] = strings.TrimRight(strings.Join(blockLines, "\n"), " \t\n")
		case ">":
			result[currentKey] = strings.TrimSpace(strings.Join(blockLines, " "))
		}
		currentKey = ""
		blockMode = ""
		blockLines = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && blockMode == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") && blockMode == "" {
			continue
		}

		if blockMode != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || trimmed == "") {
			if blockMode == "list" {
				if im := reYamlList.FindStringSubmatch(line); len(im) == 2 {
					blockLines = append(blockLines, unquoteYAML(im[1]))
				}
			} else {
				blockLines = append(blockLines, reLeading2Sp.ReplaceAllString(line, ""))
			}
			continue
		}

		if blockMode != "" && reYamlList.MatchString(line) && currentKey != "" {
			blockMode = "list"
			if im := reYamlList.FindStringSubmatch(line); len(im) == 2 {
				blockLines = append(blockLines, unquoteYAML(im[1]))
			}
			continue
		}

		flushBlock()

		kv := reYamlKV.FindStringSubmatch(line)
		if len(kv) != 3 {
			continue
		}
		key := strings.TrimSpace(kv[1])
		rawVal := strings.TrimSpace(kv[2])

		switch rawVal {
		case "|", "|-", ">", ">-":
			currentKey = key
			if strings.HasPrefix(rawVal, "|") {
				blockMode = "|"
			} else {
				blockMode = ">"
			}
			blockLines = nil
		case "":
			currentKey = key
			blockMode = "list"
			blockLines = nil
		default:
			result[key] = unquoteYAML(rawVal)
		}
	}
	flushBlock()
	return result
}

func unquoteYAML(val string) string {
	trimmed := strings.TrimSpace(val)
	if len(trimmed) >= 2 &&
		((strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`)) ||
			(strings.HasPrefix(trimmed, `'`) && strings.HasSuffix(trimmed, `'`))) {
		return trimmed[1 : len(trimmed)-1]
	}
	return trimmed
}
