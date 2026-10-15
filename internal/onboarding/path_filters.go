package onboarding

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"unicode/utf8"
)

func validatePathFilters(includes, ignores []string) error {
	if len(includes) > 100 || len(ignores) > 100 {
		return errors.New("too many webhook path filters")
	}
	for _, pattern := range append(append([]string(nil), includes...), ignores...) {
		if _, err := compileWebhookPathPattern(pattern); err != nil {
			return errors.New("path filters must be valid repository-relative glob patterns")
		}
	}
	return nil
}

func compileWebhookPathPattern(pattern string) ([]string, error) {
	if pattern == "" || len(pattern) > 256 || !utf8.ValidString(pattern) || strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, "\\\x00") {
		return nil, errors.New("invalid path pattern")
	}
	components := strings.Split(pattern, "/")
	for index, component := range components {
		if component == "" || component == "." || component == ".." || component == ".git" {
			return nil, errors.New("invalid path pattern")
		}
		// path.Match uses '^' for a negated class; the webhook UI uses the
		// more familiar '!' spelling. Rewrite only the first class character,
		// leaving a literal `[!` inside an already-open class untouched.
		component = normalizeNegatedPathClass(component)
		components[index] = component
		if component != "**" {
			if _, err := path.Match(component, "placeholder"); err != nil {
				return nil, err
			}
		}
	}
	return components, nil
}

func normalizeNegatedPathClass(pattern string) string {
	var normalized strings.Builder
	insideClass := false
	firstClassCharacter := false
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		if !insideClass && character == '[' {
			insideClass = true
			firstClassCharacter = true
			normalized.WriteByte(character)
			continue
		}
		if insideClass && firstClassCharacter && character == '!' {
			normalized.WriteByte('^')
			firstClassCharacter = false
			continue
		}
		normalized.WriteByte(character)
		if insideClass {
			if character == ']' && !firstClassCharacter {
				insideClass = false
			} else {
				firstClassCharacter = false
			}
		}
	}
	return normalized.String()
}

func webhookPathMatches(pattern, file []string) bool {
	// Dynamic programming over path components gives ** recursive directory
	// matching without translating classes or wildcard characters to regex.
	// path.Match handles each component's classes and wildcards, so a negated
	// class can never consume the separator between components.
	next := make([]bool, len(file)+1)
	next[len(file)] = true
	for patternIndex := len(pattern) - 1; patternIndex >= 0; patternIndex-- {
		current := make([]bool, len(file)+1)
		if pattern[patternIndex] == "**" {
			for fileIndex := len(file); fileIndex >= 0; fileIndex-- {
				current[fileIndex] = next[fileIndex]
				if fileIndex < len(file) {
					current[fileIndex] = current[fileIndex] || current[fileIndex+1]
				}
			}
		} else {
			for fileIndex := len(file) - 1; fileIndex >= 0; fileIndex-- {
				matched, _ := path.Match(pattern[patternIndex], file[fileIndex])
				current[fileIndex] = matched && next[fileIndex+1]
			}
		}
		next = current
	}
	return next[0]
}

func pathFiltersMatch(includes, ignores, changed []string) bool {
	if len(includes) == 0 && len(ignores) == 0 {
		return true
	}
	for _, file := range changed {
		if file == "" || strings.ContainsAny(file, "\\\x00") || !fs.ValidPath(file) {
			continue
		}
		parts := strings.Split(file, "/")
		if contains(parts, ".git") {
			continue
		}
		included := len(includes) == 0
		for _, pattern := range includes {
			matcher, err := compileWebhookPathPattern(pattern)
			if err == nil && webhookPathMatches(matcher, parts) {
				included = true
				break
			}
		}
		if !included {
			continue
		}
		ignored := false
		for _, pattern := range ignores {
			matcher, err := compileWebhookPathPattern(pattern)
			if err == nil && webhookPathMatches(matcher, parts) {
				ignored = true
				break
			}
		}
		if !ignored {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
