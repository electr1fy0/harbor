package onboarding

import "testing"

func TestWebhookPathFiltersMatchUnicodePaths(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
		file    string
		want    bool
	}{
		{name: "unicode literal", pattern: "apps/café/入口.go", file: "apps/café/入口.go", want: true},
		{name: "unicode directory glob", pattern: "apps/東京/**", file: "apps/東京/server.go", want: true},
		{name: "unicode glob mismatch", pattern: "apps/café/**", file: "apps/naïve/server.go", want: false},
		{name: "globstar matches newline path components", pattern: "apps/**/file.go", file: "apps/a\nb/file.go", want: true},
		{name: "globstar matches zero directories", pattern: "apps/**/file.go", file: "apps/file.go", want: true},
		{name: "negated classes cannot match path separators", pattern: "apps/[!a][!a]file.go", file: "apps/b/file.go", want: false},
		{name: "negated class excludes its listed character", pattern: "apps/[!a]file.go", file: "apps/afile.go", want: false},
		{name: "class literal bracket and bang remain members", pattern: "apps/[[!a]file.go", file: "apps/[file.go", want: true},
		{name: "class literal bang remains a member", pattern: "apps/[[!a]file.go", file: "apps/!file.go", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pathFiltersMatch([]string{test.pattern}, nil, []string{test.file}); got != test.want {
				t.Fatalf("pathFiltersMatch(%q, %q) = %t, want %t", test.pattern, test.file, got, test.want)
			}
		})
	}
}
