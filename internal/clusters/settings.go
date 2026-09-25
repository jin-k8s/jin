// Package clusters stores per-cluster settings: environment, GitOps repository and cloud identity.
package clusters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/jin-k8s/jin/internal/gitops"
	"github.com/jin-k8s/jin/internal/upgrade"
)

type GitOps struct {
	Provider   string `json:"provider"` // "github"
	BaseURL    string `json:"baseUrl,omitempty"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	BaseBranch string `json:"baseBranch,omitempty"`
	// TokenEnv names the environment variable holding the token. Tokens are never stored.
	TokenEnv            string          `json:"tokenEnv,omitempty"`
	Targets             []gitops.Target `json:"targets"`
	ApplyTimeoutMinutes int             `json:"applyTimeoutMinutes,omitempty"`
}

func (g *GitOps) Token() string {
	name := g.TokenEnv
	if name == "" {
		name = "GITHUB_TOKEN"
	}
	return os.Getenv(name)
}

type Settings struct {
	Context string `json:"context"`
	// Environment (e.g. prod, staging) is used by approval policies.
	Environment string          `json:"environment,omitempty"`
	GitOps      *GitOps         `json:"gitops,omitempty"`
	GKE         *upgrade.GKERef `json:"gke,omitempty"`
	AKS         *upgrade.AKSRef `json:"aks,omitempty"`
}

var (
	ownerRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	// Token variables are restricted so settings cannot point Jin at unrelated secrets
	// (e.g. AWS_SECRET_ACCESS_KEY) and send them to a repository host.
	envName  = regexp.MustCompile(`^(GITHUB|JIN_GITHUB)_[A-Z0-9_]+$`)
	envLabel = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
)

func (s *Settings) Validate() error {
	if !envLabel.MatchString(s.Environment) {
		return fmt.Errorf("environment must be lowercase letters, digits and dashes")
	}
	if g := s.GitOps; g != nil {
		if g.Provider != "github" {
			return fmt.Errorf("unsupported GitOps provider %q (github)", g.Provider)
		}
		if !ownerRepo.MatchString(g.Owner) || !ownerRepo.MatchString(g.Repo) {
			return errors.New("invalid repository owner or name")
		}
		if g.TokenEnv != "" && !envName.MatchString(g.TokenEnv) {
			return errors.New("tokenEnv must be an environment variable starting with GITHUB_ or JIN_GITHUB_")
		}
		for _, t := range g.Targets {
			if err := t.Validate(); err != nil {
				return err
			}
		}
	}
	if a := s.AKS; a != nil && (a.SubscriptionID == "" || a.ResourceGroup == "" || a.Name == "") {
		return errors.New("AKS settings need subscriptionId, resourceGroup and name")
	}
	if g := s.GKE; g != nil && (g.Project == "" || g.Location == "" || g.Name == "") {
		return errors.New("GKE settings need project, location and name")
	}
	return nil
}

// Store keeps settings in one JSON file (0600).
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

type file struct {
	Version  int                  `json:"version"`
	Clusters map[string]*Settings `json:"clusters"`
}

func (s *Store) load() (*file, error) {
	f := &file{Version: 1, Clusters: map[string]*Settings{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", s.path, err)
	}
	if f.Clusters == nil {
		f.Clusters = map[string]*Settings{}
	}
	return f, nil
}

func (s *Store) Get(context string) (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	if st, ok := f.Clusters[context]; ok {
		return st, nil
	}
	return &Settings{Context: context}, nil
}

func (s *Store) List() ([]*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]*Settings, 0, len(f.Clusters))
	for _, st := range f.Clusters {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out, nil
}

func (s *Store) Put(st *Settings) error {
	if err := st.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Clusters[st.Context] = st
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".clusters-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
