package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicGitHubBrowseAndPrivateDetails(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if strings.HasPrefix(r.URL.Path, "/users/") {
			if r.Header.Get("Authorization") != "" {
				t.Error("public browse unexpectedly sends a credential")
			}
			if r.URL.Query().Get("type") != "public" {
				t.Error("missing public repository filter")
			}
			json.NewEncoder(w).Encode([]map[string]any{{"full_name": "org/example", "name": "example", "default_branch": "main", "html_url": "https://evil.example/"}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("private details missing daemon token")
		}
		if strings.HasSuffix(r.URL.Path, "/branches") {
			fmt.Fprint(w, `[{"name":"main","commit":{"sha":"abcd"}}]`)
		} else {
			fmt.Fprint(w, `{"full_name":"org/example","name":"example","private":true,"default_branch":"main"}`)
		}
	}))
	defer server.Close()
	api := NewGitHubAPI()
	api.BaseURL = server.URL
	api.Client = server.Client()
	repos, err := api.PublicRepositories(context.Background(), "org", "")
	if err != nil || len(repos) != 1 || repos[0].URL != "https://github.com/org/example" {
		t.Fatalf("repos=%+v err=%v", repos, err)
	}
	repo, err := api.Repository(context.Background(), "org", "example", "secret-token")
	if err != nil || !repo.Private || len(repo.Branches) != 1 || repo.Branches[0].CommitSHA != "abcd" {
		t.Fatalf("repo=%+v err=%v", repo, err)
	}
	before := len(paths)
	for _, name := range []string{"..", "bad/name", "evil?x=1", "bad%2frepo"} {
		if _, err := api.Repository(context.Background(), "org", name, "secret-token"); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if len(paths) != before {
		t.Error("invalid coordinates reached GitHub")
	}
}

func TestGitHubBrowsePaginationAndOptionalBranches(t *testing.T) {
	var pages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/branches") {
			http.Error(w, "private failure details", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			fmt.Fprint(w, `{"full_name":"org/example","default_branch":"main"}`)
			return
		}
		pages++
		repos := make([]map[string]any, 0)
		if r.URL.Query().Get("page") == "1" {
			for i := 0; i < 100; i++ {
				repos = append(repos, map[string]any{"full_name": fmt.Sprintf("org/repo%d", i), "default_branch": "main"})
			}
		}
		json.NewEncoder(w).Encode(repos)
	}))
	defer server.Close()
	api := NewGitHubAPI()
	api.BaseURL = server.URL
	api.Client = server.Client()
	repos, err := api.PublicRepositories(context.Background(), "org", "")
	if err != nil || len(repos) != 100 || pages != 2 {
		t.Fatalf("repos=%d pages=%d err=%v", len(repos), pages, err)
	}
	repo, err := api.Repository(context.Background(), "org", "example", "")
	if err != nil || repo.FullName != "org/example" || repo.Branches != nil {
		t.Fatalf("metadata lost on optional branches failure: %+v %v", repo, err)
	}
}

func TestGitHubRepositoryListReportsWhenPageCapWouldTruncateResults(t *testing.T) {
	for _, test := range []struct {
		name      string
		overflow  bool
		wantCount int
		wantErr   bool
	}{
		{name: "exactly at cap", wantCount: 1000},
		{name: "above cap", overflow: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var pages int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := 0
				_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
				pages++
				count := 100
				if page == 11 {
					count = 0
					if test.overflow {
						count = 1
					}
				}
				repositories := make([]map[string]any, count)
				for i := range repositories {
					repositories[i] = map[string]any{
						"full_name":      fmt.Sprintf("org/repo-%d-%d", page, i),
						"default_branch": "main",
					}
				}
				_ = json.NewEncoder(w).Encode(repositories)
			}))
			defer server.Close()
			api := NewGitHubAPI()
			api.BaseURL, api.Client = server.URL, server.Client()
			repositories, err := api.PublicRepositories(context.Background(), "org", "")
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%t", err, test.wantErr)
			}
			if test.wantErr {
				if !strings.Contains(err.Error(), "1,000-repository limit") {
					t.Fatalf("unexpected overflow error: %v", err)
				}
			} else if len(repositories) != test.wantCount {
				t.Fatalf("repository count=%d, want %d", len(repositories), test.wantCount)
			}
			if pages != 11 {
				t.Fatalf("requested %d pages, want 11", pages)
			}
		})
	}
}
