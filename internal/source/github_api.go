package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultGitHubAPIURL = "https://api.github.com"
	maxGitHubBody       = 2 << 20
)

// GitHubRepository contains the public metadata Harbor needs to display a
// repository picker. It never contains credentials or file contents.
type GitHubRepository struct {
	FullName      string    `json:"fullName"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	URL           string    `json:"url"`
	DefaultBranch string    `json:"defaultBranch"`
	Language      string    `json:"language,omitempty"`
	Private       bool      `json:"private"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
}

type GitHubUser struct {
	Login string `json:"login"`
}

type GitHubAPI struct {
	Client  *http.Client
	BaseURL string
}

func NewGitHubAPI() *GitHubAPI {
	return &GitHubAPI{Client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, BaseURL: defaultGitHubAPIURL}
}

func (api *GitHubAPI) endpoint(path string) (string, error) {
	base := strings.TrimRight(api.BaseURL, "/")
	if base == "" {
		base = defaultGitHubAPIURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("GitHub API URL is invalid")
	}
	return base + path, nil
}

func (api *GitHubAPI) request(ctx context.Context, token, path string, output any) error {
	if ctx == nil {
		return errors.New("GitHub request context must not be nil")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("GitHub token contains an invalid character")
	}
	endpoint, err := api.endpoint(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("could not create GitHub request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "Harbor")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := api.Client
	if client == nil {
		client = NewGitHubAPI().Client
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("could not reach GitHub")
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxGitHubBody+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxGitHubBody {
		return errors.New("GitHub response was unavailable or too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("GitHub rejected the token")
		case http.StatusForbidden:
			return errors.New("GitHub token lacks access to the requested repositories")
		case http.StatusNotFound:
			return errors.New("GitHub resource was not found or is not accessible")
		default:
			return fmt.Errorf("GitHub request failed (HTTP %d)", response.StatusCode)
		}
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("GitHub returned invalid data")
	}
	return nil
}

func (api *GitHubAPI) AuthenticatedUser(ctx context.Context, token string) (GitHubUser, error) {
	var user GitHubUser
	if err := api.request(ctx, token, "/user", &user); err != nil {
		return GitHubUser{}, err
	}
	if !validGitHubLogin(user.Login) {
		return GitHubUser{}, errors.New("GitHub returned invalid account metadata")
	}
	return user, nil
}

func (api *GitHubAPI) Repositories(ctx context.Context, token string) ([]GitHubRepository, error) {
	if token == "" {
		return nil, errors.New("connect a GitHub account to browse private and organization repositories")
	}
	const pageSize = 100
	result := make([]GitHubRepository, 0)
	for page := 1; page <= 10; page++ {
		path := fmt.Sprintf("/user/repos?visibility=all&affiliation=owner%%2Ccollaborator%%2Corganization_member&per_page=%d&page=%d&sort=updated", pageSize, page)
		var response []struct {
			Name          string `json:"name"`
			FullName      string `json:"full_name"`
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
			Description   string `json:"description"`
			Language      string `json:"language"`
			Private       bool   `json:"private"`
			UpdatedAt     string `json:"updated_at"`
		}
		if err := api.request(ctx, token, path, &response); err != nil {
			return nil, err
		}
		for _, repo := range response {
			if !validRepositoryFullName(repo.FullName) || repo.DefaultBranch == "" {
				continue
			}
			parts := strings.SplitN(repo.FullName, "/", 2)
			item := GitHubRepository{
				FullName: repo.FullName, Name: repo.Name,
				URL: "https://github.com/" + repo.FullName, DefaultBranch: repo.DefaultBranch,
				Description: boundedText(repo.Description, 1000), Language: boundedText(repo.Language, 100), Private: repo.Private,
			}
			if len(parts) == 2 && repo.Name != parts[1] {
				item.Name = parts[1]
			}
			if updatedAt, parseErr := time.Parse(time.RFC3339, repo.UpdatedAt); parseErr == nil {
				item.UpdatedAt = updatedAt.UTC()
			}
			result = append(result, item)
		}
		if len(response) < pageSize {
			break
		}
	}
	return result, nil
}

func validGitHubLogin(login string) bool {
	if login == "" || len(login) > 39 || login[0] == '-' || login[len(login)-1] == '-' || strings.Contains(login, "--") {
		return false
	}
	for _, c := range login {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validRepositoryFullName(fullName string) bool {
	parts := strings.Split(fullName, "/")
	return len(parts) == 2 && validGitHubLogin(parts[0]) && len(parts[1]) > 0 && len(parts[1]) <= 100 && !strings.ContainsAny(parts[1], " /\\\x00")
}

func boundedText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
