package onboarding

import (
	"errors"
	"io/fs"
	"path"
	"regexp"
	"strings"
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

func compileWebhookPathPattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || len(pattern) > 256 || strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, "\\\x00") {
		return nil, errors.New("invalid path pattern")
	}
	for _, component := range strings.Split(pattern, "/") {
		if component == "" || component == "." || component == ".." || component == ".git" {
			return nil, errors.New("invalid path pattern")
		}
	}
	if _, err := path.Match(pattern, "placeholder"); err != nil {
		return nil, err
	}
	var expression strings.Builder
	expression.WriteByte('^')
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					expression.WriteString("(?:.*/)?")
					i++
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				return nil, errors.New("invalid path pattern")
			}
			end += i + 1
			class := pattern[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			i = end
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
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
			if err == nil && matcher.MatchString(file) {
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
			if err == nil && matcher.MatchString(file) {
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
