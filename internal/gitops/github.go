package gitops

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
	"path"
	"strings"
	"time"
)

type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   string `json:"head"`
	Base   string `json:"base"`
}

// Repo is the git hosting API surface the GitOps executor needs. No local clone is used.
type Repo interface {
	DefaultBranch(ctx context.Context) (string, error)
	HeadSHA(ctx context.Context, branch string) (string, error)
	Tree(ctx context.Context, ref string) ([]string, error)
	// ReadFile returns the content and the blob SHA of a file at ref.
	ReadFile(ctx context.Context, ref, file string) ([]byte, string, error)
	// CreateBranch is a no-op when the branch already exists.
	CreateBranch(ctx context.Context, name, sha string) error
	WriteFile(ctx context.Context, branch, file, message string, content []byte, blobSHA string) error
	// FindPR returns the most recent pull request for head in any state, or nil.
	FindPR(ctx context.Context, head string) (*PR, error)
	OpenPR(ctx context.Context, head, base, title, body string) (*PR, error)
	GetPR(ctx context.Context, number int) (*PR, error)
}

var ErrNotFound = errors.New("not found")

// GitHub implements Repo with the GitHub REST API (github.com or GitHub Enterprise Server).
type GitHub struct {
	BaseURL string // https://api.github.com or https://ghe.example.com/api/v3
	Owner   string
	Name    string
	Token   string
	HTTP    *http.Client
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GitHub) base() string {
	if g.BaseURL == "" {
		return "https://api.github.com"
	}
	return strings.TrimRight(g.BaseURL, "/")
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (g *GitHub) do(ctx context.Context, method, p string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base()+"/repos/"+url.PathEscape(g.Owner)+"/"+url.PathEscape(g.Name)+p, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GitHub %s %s: %w", method, p, ErrNotFound)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{Status: resp.StatusCode, Message: e.Message, Op: method + " " + p}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type apiError struct {
	Status  int
	Message string
	Op      string
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub %s: %d %s", e.Op, e.Status, e.Message) }

func (g *GitHub) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, "", nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

func (g *GitHub) HeadSHA(ctx context.Context, branch string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/ref/heads/"+escapePath(branch), nil, &r); err != nil {
		return "", err
	}
	return r.Object.SHA, nil
}

func (g *GitHub) Tree(ctx context.Context, ref string) ([]string, error) {
	var r struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/trees/"+url.PathEscape(ref)+"?recursive=1", nil, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range r.Tree {
		if e.Type == "blob" {
			out = append(out, e.Path)
		}
	}
	if r.Truncated {
		return out, errors.New("repository tree is too large and was truncated by GitHub; configure targets manually")
	}
	return out, nil
}

func (g *GitHub) ReadFile(ctx context.Context, ref, file string) ([]byte, string, error) {
	var r struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
		Type     string `json:"type"`
	}
	if err := g.do(ctx, http.MethodGet, "/contents/"+escapePath(file)+"?ref="+url.QueryEscape(ref), nil, &r); err != nil {
		return nil, "", err
	}
	if r.Type != "file" || r.Encoding != "base64" {
		return nil, "", fmt.Errorf("%s is not a regular file under 1 MB", file)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(r.Content, "\n", ""))
	return b, r.SHA, err
}

func (g *GitHub) CreateBranch(ctx context.Context, name, sha string) error {
	err := g.do(ctx, http.MethodPost, "/git/refs", map[string]string{"ref": "refs/heads/" + name, "sha": sha}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity && strings.Contains(ae.Message, "already exists") {
		return nil
	}
	return err
}

func (g *GitHub) WriteFile(ctx context.Context, branch, file, message string, content []byte, blobSHA string) error {
	body := map[string]string{"message": message, "content": base64.StdEncoding.EncodeToString(content), "branch": branch}
	if blobSHA != "" {
		body["sha"] = blobSHA
	}
	return g.do(ctx, http.MethodPut, "/contents/"+escapePath(file), body, nil)
}

type ghPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	// List responses omit "merged"; merged_at is set instead.
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p ghPR) pr() *PR {
	return &PR{Number: p.Number, URL: p.HTMLURL, State: p.State, Merged: p.Merged || p.MergedAt != nil, Head: p.Head.Ref, Base: p.Base.Ref}
}

func (g *GitHub) FindPR(ctx context.Context, head string) (*PR, error) {
	var prs []ghPR
	q := "/pulls?state=all&per_page=10&head=" + url.QueryEscape(g.Owner+":"+head)
	if err := g.do(ctx, http.MethodGet, q, nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return prs[0].pr(), nil
}

func (g *GitHub) OpenPR(ctx context.Context, head, base, title, body string) (*PR, error) {
	var p ghPR
	if err := g.do(ctx, http.MethodPost, "/pulls", map[string]string{"head": head, "base": base, "title": title, "body": body}, &p); err != nil {
		return nil, err
	}
	return p.pr(), nil
}

func (g *GitHub) GetPR(ctx context.Context, number int) (*PR, error) {
	var p ghPR
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/pulls/%d", number), nil, &p); err != nil {
		return nil, err
	}
	return p.pr(), nil
}

// RepoWorkspace reads a repository at a fixed ref, caching the tree and file contents.
type RepoWorkspace struct {
	ctx   context.Context
	repo  Repo
	ref   string
	tree  []string
	cache map[string][]byte
}

func NewRepoWorkspace(ctx context.Context, repo Repo, ref string) (*RepoWorkspace, error) {
	tree, err := repo.Tree(ctx, ref)
	if err != nil && tree == nil {
		return nil, err
	}
	return &RepoWorkspace{ctx: ctx, repo: repo, ref: ref, tree: tree, cache: map[string][]byte{}}, err
}

func (w *RepoWorkspace) Files() []string { return w.tree }

func (w *RepoWorkspace) Read(file string) ([]byte, error) {
	if b, ok := w.cache[file]; ok {
		return b, nil
	}
	b, _, err := w.repo.ReadFile(w.ctx, w.ref, file)
	if err != nil {
		return nil, err
	}
	w.cache[file] = b
	return b, nil
}

func (w *RepoWorkspace) List(dir string) ([]string, error) {
	var out []string
	for _, f := range w.tree {
		if path.Dir(f) == dir {
			out = append(out, f)
		}
	}
	return out, nil
}

// overlay applies pending edits on top of a workspace so several targets can edit one file.
type overlay struct {
	base  Workspace
	edits map[string][]byte
}

func (o *overlay) Read(file string) ([]byte, error) {
	if b, ok := o.edits[file]; ok {
		return b, nil
	}
	return o.base.Read(file)
}

func (o *overlay) List(dir string) ([]string, error) { return o.base.List(dir) }
