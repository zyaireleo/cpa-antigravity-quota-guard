package config

import (
	"fmt"
	"strconv"
	"strings"
)

func extractPluginConfigYAML(data string) string {
	if hasTopLevelPluginField(data) {
		return data
	}
	lines := strings.Split(data, "\n")
	hasHostPlugins := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "plugins:" && leadingSpaces(line) == 0 {
			hasHostPlugins = true
			break
		}
	}

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "antigravity-priority:" && trimmed != PluginID+":" {
			continue
		}
		if !isCPAPluginConfigPath(lines, index) {
			continue
		}
		indent := leadingSpaces(line)
		collected := collectIndentedBlock(lines[index+1:], indent)
		if len(collected) == 0 {
			return ""
		}
		return strings.Join(collected, "\n")
	}

	// If this is a full CPA config.yaml that doesn't define a recognized plugin block,
	// return empty so Default() configuration is used instead of failing on host-level fields.
	if hasHostPlugins {
		return ""
	}
	return data
}

func hasTopLevelPluginField(data string) bool {
	for _, line := range strings.Split(data, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || leadingSpaces(line) != 0 {
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case KeyEnabled, KeyAutoApply, KeyStateCachePath, KeyMode, KeyManagedAuth,
			KeyEnforcedGroups, KeyRequireUniformPriority, KeyProbeInterval,
			KeyEvidenceMaxAge, KeyGeneric429, KeyHalfOpenLease,
			KeyUnknownAuthPolicy, KeyUnknownModelPolicy, KeyStatePath, "required-scheduler-for":
			return true
		}
	}
	return false
}

func isCPAPluginConfigPath(lines []string, pluginIndex int) bool {
	pluginIndent := leadingSpaces(lines[pluginIndex])
	configsIndent := -1
	for index := pluginIndex - 1; index >= 0; index-- {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(lines[index])
		if indent >= pluginIndent {
			continue
		}
		if configsIndent < 0 {
			if trimmed != "configs:" {
				return false
			}
			configsIndent = indent
			continue
		}
		return trimmed == "plugins:" && indent < configsIndent
	}
	return false
}

func collectIndentedBlock(lines []string, parentIndent int) []string {
	collected := make([]string, 0)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(line)
		if indent <= parentIndent {
			break
		}
		collected = append(collected, line)
	}
	return normalizeIndentedBlock(collected)
}

func normalizeIndentedBlock(lines []string) []string {
	baseIndent := -1
	for _, line := range lines {
		indent := leadingSpaces(line)
		if baseIndent < 0 || indent < baseIndent {
			baseIndent = indent
		}
	}
	if baseIndent <= 0 {
		return lines
	}
	normalized := make([]string, 0, len(lines))
	for _, line := range lines {
		if leadingSpaces(line) < baseIndent {
			normalized = append(normalized, strings.TrimLeft(line, " "))
			continue
		}
		normalized = append(normalized, line[baseIndent:])
	}
	return normalized
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func parseYAMLMap(data string) (map[string]any, error) {
	result := map[string]any{}
	lines := strings.Split(data, "\n")
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent != 0 {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, invalid("config", trimmed, "must use key: value syntax")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if value != "" {
			result[key] = yamlValue(key, value)
			continue
		}

		block, next := collectYAMLChildBlock(lines, index+1)
		if len(block) == 0 {
			result[key] = ""
			continue
		}
		index = next - 1
		if strings.HasPrefix(strings.TrimSpace(block[0]), "-") {
			items := make([]string, 0, len(block))
			for _, child := range block {
				item := strings.TrimSpace(child)
				if !strings.HasPrefix(item, "-") {
					return nil, invalid(key, item, "must be a YAML list")
				}
				item = strings.TrimSpace(strings.TrimPrefix(item, "-"))
				if item != "" {
					items = append(items, yamlText(item))
				}
			}
			result[key] = items
			continue
		}
		nested, err := parseYAMLMap(strings.Join(normalizeIndentedBlock(block), "\n"))
		if err != nil {
			return nil, err
		}
		result[key] = nested
	}
	return result, nil
}

func collectYAMLChildBlock(lines []string, start int) ([]string, int) {
	block := make([]string, 0)
	for index := start; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if leadingSpaces(line) == 0 {
			return block, index
		}
		block = append(block, line)
	}
	return block, len(lines)
}

func yamlValue(key, value string) any {
	if key == KeyEnforcedGroups {
		text := yamlText(value)
		text = strings.TrimPrefix(text, "[")
		text = strings.TrimSuffix(text, "]")
		if strings.TrimSpace(text) == "" {
			return []string{}
		}
		items := make([]string, 0)
		for _, item := range strings.Split(text, ",") {
			item = strings.Trim(strings.TrimSpace(item), "\"'")
			if item != "" {
				items = append(items, item)
			}
		}
		return items
	}
	return yamlScalar(value)
}

func yamlScalar(value string) any {
	text := yamlText(value)
	if parsed, err := strconv.Atoi(text); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseBool(text); err == nil {
		return parsed
	}
	return text
}

func yamlText(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 1 && trimmed[0] == trimmed[len(trimmed)-1] && (trimmed[0] == '"' || trimmed[0] == '\'') {
		return trimmed[1 : len(trimmed)-1]
	}
	return trimmed
}

func invalid(field string, value string, reason string) error {
	return fmt.Errorf("%w: %s=%q %s", ErrInvalidConfig, field, value, reason)
}
