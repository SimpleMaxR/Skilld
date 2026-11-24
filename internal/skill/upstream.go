package skill

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxGitHubFileSize = 2 * 1024 * 1024

var ignoredSegments = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"__pycache__":  {},
	".DS_Store":    {},
}

// ParsedGitHubURL holds normalized components of a GitHub repository or subdirectory link.
type ParsedGitHubURL struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref,omitempty"`
	Subpath      string `json:"subpath"`
	CanonicalURL string `json:"canonicalUrl"`
}

// UpstreamCheckResult holds the latest commit metadata for a GitHub skill path.
type UpstreamCheckResult struct {
	LatestRev     string `json:"latestRev"`
	LatestMessage string `json:"latestMessage"`
	LatestAt      string `json:"latestAt"`
}

// FetchedGitHubSkill represents a fully fetched and hashed skill directory from GitHub.
type FetchedGitHubSkill struct {
	Rev         string
	Message     string
	CheckedAt   string
	Files       map[string]FileEntry
	ContentHash string
	Meta        SkillMeta
}

// UpstreamError preserves HTTP status and error code when GitHub calls fail.
type UpstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	return e.Message
}

var reGitHubHost = regexp.MustCompile(`(?i)^(www\.)?github\.com$`)

// ParseGitHubURL parses a GitHub URL or shorthand (owner/repo[/subpath]).
func ParseGitHubURL(input, explicitRef, explicitSubpath string) (ParsedGitHubURL, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return ParsedGitHubURL{}, &UpstreamError{Status: 400, Code: "INVALID_GITHUB_URL", Message: "GitHub URL is required"}
	}

	var owner, repo string
	ref := strings.TrimSpace(explicitRef)
	subpath := cleanSubpath(explicitSubpath)

	if strings.HasPrefix(strings.ToLower(raw), "http://") || strings.HasPrefix(strings.ToLower(raw), "https://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ParsedGitHubURL{}, &UpstreamError{Status: 400, Code: "INVALID_GITHUB_URL", Message: "Malformed GitHub URL"}
		}
		if !reGitHubHost.MatchString(u.Hostname()) {
			return ParsedGitHubURL{}, &UpstreamError{Status: 400, Code: "INVALID_GITHUB_URL", Message: "Only github.com URLs are supported"}
		}
		parts := splitNonEmpty(u.Path)
		if len(parts) < 2 {
			return ParsedGitHubURL{}, &UpstreamError{Status: 400, Code: "INVALID_GITHUB_URL", Message: "GitHub URL must include owner and repository"}
		}
		owner = parts[0]
		repo = trimDotGit(parts[1])

		if len(parts) >= 4 && parts[2] == "tree" {
			if ref == "" {
				if dec, err := url.PathUnescape(parts[3]); err == nil {
					ref = dec
				} else {
					ref = parts[3]
				}
			}
			if subpath == "" && len(parts) > 4 {
				rest := make([]string, 0, len(parts)-4)
				for _, seg := range parts[4:] {
					if dec, err := url.PathUnescape(seg); err == nil {
						rest = append(rest, dec)
					} else {
						rest = append(rest, seg)
					}
				}
				subpath = cleanSubpath(strings.Join(rest, "/"))
			}
		} else if len(parts) >= 5 && parts[2] == "blob" {
			if ref == "" {
				if dec, err := url.PathUnescape(parts[3]); err == nil {
					ref = dec
				} else {
					ref = parts[3]
				}
			}
			rest := make([]string, 0, len(parts)-4)
			for _, seg := range parts[4:] {
				if dec, err := url.PathUnescape(seg); err == nil {
					rest = append(rest, dec)
				} else {
					rest = append(rest, seg)
				}
			}
			if len(rest) > 0 && rest[len(rest)-1] == "SKILL.md" {
				rest = rest[:len(rest)-1]
			}
			if subpath == "" {
				subpath = cleanSubpath(strings.Join(rest, "/"))
			}
		}
	} else {
		parts := splitNonEmpty(raw)
		if len(parts) < 2 {
			return ParsedGitHubURL{}, &UpstreamError{
				Status:  400,
				Code:    "INVALID_GITHUB_URL",
				Message: "Provide a GitHub URL or owner/repo[/subpath]",
			}
		}
		owner = parts[0]
		repo = trimDotGit(parts[1])
		if subpath == "" && len(parts) > 2 {
			subpath = cleanSubpath(strings.Join(parts[2:], "/"))
		}
	}

	return ParsedGitHubURL{
		Owner:        owner,
		Repo:         repo,
		Ref:          ref,
		Subpath:      subpath,
		CanonicalURL: buildCanonicalGitHubURL(owner, repo, ref, subpath),
	}, nil
}

func buildCanonicalGitHubURL(owner, repo, ref, subpath string) string {
	base := fmt.Sprintf("https://github.com/%s/%s", owner, repo)
	if subpath == "" && ref == "" {
		return base
	}
	r := ref
	if r == "" {
		r = "HEAD"
	}
	if subpath != "" {
		return fmt.Sprintf("%s/tree/%s/%s", base, r, subpath)
	}
	return fmt.Sprintf("%s/tree/%s", base, r)
}

func cleanSubpath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

func splitNonEmpty(p string) []string {
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimDotGit(repo string) string {
	if strings.HasSuffix(strings.ToLower(repo), ".git") {
		return repo[:len(repo)-4]
	}
	return repo
}

// GitHubClient fetches commit and git tree contents from the GitHub REST API.
type GitHubClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewGitHubClient creates a GitHubClient with sensible defaults.
func NewGitHubClient(token string) *GitHubClient {
	return &GitHubClient{
		BaseURL: "https://api.github.com",
		Token:   strings.TrimSpace(token),
		HTTPClient: &http.Client{
			Timeout: 25 * time.Second,
		},
	}
}

func (c *GitHubClient) fetchJSON(ctx context.Context, rawURL, tokenOverride string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &UpstreamError{Status: 500, Code: "GITHUB_REQUEST_ERROR", Message: err.Error()}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Skilld/0.2")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	tok := strings.TrimSpace(tokenOverride)
	if tok == "" {
		tok = c.Token
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return &UpstreamError{Status: 502, Code: "GITHUB_API_ERROR", Message: fmt.Sprintf("GitHub API network error: %v", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return &UpstreamError{Status: 502, Code: "GITHUB_API_ERROR", Message: fmt.Sprintf("Read GitHub response: %v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := 502
		if resp.StatusCode == 404 {
			status = 404
		}
		return &UpstreamError{
			Status:  status,
			Code:    "GITHUB_API_ERROR",
			Message: fmt.Sprintf("GitHub API error (%d): %s", resp.StatusCode, extractGitHubError(body)),
		}
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return &UpstreamError{Status: 502, Code: "GITHUB_API_ERROR", Message: fmt.Sprintf("Decode GitHub JSON: %v", err)}
	}
	return nil
}

func extractGitHubError(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return parsed.Message
	}
	if len(body) > 160 {
		return string(body[:160])
	}
	if len(body) == 0 {
		return "Request failed"
	}
	return string(body)
}

// CheckUpstream queries the latest commit SHA for a GitHub repository/subpath.
func (c *GitHubClient) CheckUpstream(ctx context.Context, owner, repo, ref, subpath, tokenOverride string) (UpstreamCheckResult, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}

	qs := url.Values{}
	qs.Set("per_page", "1")
	if ref != "" {
		qs.Set("sha", ref)
	}
	if subpath != "" {
		qs.Set("path", subpath)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits?%s",
		base,
		url.PathEscape(owner),
		url.PathEscape(repo),
		qs.Encode(),
	)

	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
			Author struct {
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := c.fetchJSON(ctx, endpoint, tokenOverride, &commits); err != nil {
		return UpstreamCheckResult{}, err
	}
	if len(commits) == 0 {
		suffix := ""
		if subpath != "" {
			suffix = "/" + subpath
		}
		return UpstreamCheckResult{}, &UpstreamError{
			Status:  404,
			Code:    "GITHUB_PATH_NOT_FOUND",
			Message: fmt.Sprintf("No commits found for %s/%s%s", owner, repo, suffix),
		}
	}

	latest := commits[0]
	firstLine := strings.Split(strings.ReplaceAll(latest.Commit.Message, "\r\n", "\n"), "\n")[0]
	if len(firstLine) > 200 {
		firstLine = firstLine[:200]
	}
	latestAt := latest.Commit.Committer.Date
	if latestAt == "" {
		latestAt = latest.Commit.Author.Date
	}
	if latestAt == "" {
		latestAt = ISONow()
	}

	return UpstreamCheckResult{
		LatestRev:     latest.SHA,
		LatestMessage: firstLine,
		LatestAt:      latestAt,
	}, nil
}

// FetchSkillFiles downloads all files in a GitHub skill directory and hashes them canonically.
func (c *GitHubClient) FetchSkillFiles(ctx context.Context, owner, repo, ref, subpath, tokenOverride string) (FetchedGitHubSkill, error) {
	upstream, err := c.CheckUpstream(ctx, owner, repo, ref, subpath, tokenOverride)
	if err != nil {
		return FetchedGitHubSkill{}, err
	}

	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}

	treeEndpoint := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1",
		base,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(upstream.LatestRev),
	)
	var treeData struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Mode string `json:"mode"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := c.fetchJSON(ctx, treeEndpoint, tokenOverride, &treeData); err != nil {
		return FetchedGitHubSkill{}, err
	}

	prefix := ""
	if cleaned := cleanSubpath(subpath); cleaned != "" {
		prefix = cleaned + "/"
	}

	type blobJob struct {
		rel  string
		sha  string
		mode string
	}
	var jobs []blobJob
	for _, item := range treeData.Tree {
		if item.Type != "blob" {
			continue
		}
		if item.Size > maxGitHubFileSize {
			continue
		}
		if prefix != "" && !strings.HasPrefix(item.Path, prefix) {
			continue
		}
		rel := item.Path
		if prefix != "" {
			rel = strings.TrimPrefix(item.Path, prefix)
		}
		if rel == "" {
			continue
		}
		skip := false
		for _, seg := range strings.Split(rel, "/") {
			if _, banned := ignoredSegments[seg]; banned {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		jobs = append(jobs, blobJob{rel: rel, sha: item.SHA, mode: item.Mode})
	}

	if len(jobs) == 0 {
		displayPath := subpath
		if displayPath == "" {
			displayPath = "/"
		}
		return FetchedGitHubSkill{}, &UpstreamError{
			Status:  404,
			Code:    "GITHUB_EMPTY_SKILL",
			Message: fmt.Sprintf("No files found at GitHub path %q", displayPath),
		}
	}

	rawFiles := make(map[string]FileEntry, len(jobs))
	var mu sync.Mutex
	var firstErr error
	var errOnce sync.Once
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup

	for _, job := range jobs {
		wg.Add(1)
		go func(j blobJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			blobURL := fmt.Sprintf("%s/repos/%s/%s/git/blobs/%s",
				base,
				url.PathEscape(owner),
				url.PathEscape(repo),
				url.PathEscape(j.sha),
			)
			var blob struct {
				Encoding string `json:"encoding"`
				Content  string `json:"content"`
			}
			if err := c.fetchJSON(ctx, blobURL, tokenOverride, &blob); err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			enc, content := decodeGitHubBlob(blob.Content, blob.Encoding)
			mode := "0644"
			if j.mode == "100755" {
				mode = "0755"
			}
			mu.Lock()
			rawFiles[j.rel] = FileEntry{Encoding: enc, Content: content, Mode: mode}
			mu.Unlock()
		}(job)
	}
	wg.Wait()

	if firstErr != nil {
		return FetchedGitHubSkill{}, firstErr
	}

	files, err := NormalizeFileMap(rawFiles)
	if err != nil {
		return FetchedGitHubSkill{}, &UpstreamError{Status: 400, Code: "INVALID_SKILL_FILES", Message: err.Error()}
	}
	contentHash, err := ComputeFilesHash(files)
	if err != nil {
		return FetchedGitHubSkill{}, err
	}

	var meta SkillMeta
	if skillMd, ok := files["SKILL.md"]; ok && skillMd.Encoding == "utf8" {
		meta = ParseSkillFrontmatter(skillMd.Content)
	}

	return FetchedGitHubSkill{
		Rev:         upstream.LatestRev,
		Message:     upstream.LatestMessage,
		CheckedAt:   ISONow(),
		Files:       files,
		ContentHash: contentHash,
		Meta:        meta,
	}, nil
}

func decodeGitHubBlob(content, encoding string) (string, string) {
	if encoding != "" && encoding != "base64" {
		return "utf8", content
	}
	cleanB64 := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, content)
	decoded, err := base64.StdEncoding.DecodeString(cleanB64)
	if err != nil {
		return "base64", cleanB64
	}
	if !bytes.ContainsRune(decoded, 0) && utf8.Valid(decoded) {
		return "utf8", string(decoded)
	}
	return "base64", cleanB64
}

// IsUpstreamError extracts UpstreamError if wrapped.
func IsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}
