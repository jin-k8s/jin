// Package runrecord persists a structured record of every Jin run. Records are the contract
// consumed by the Admin UI and control plane; change the schema only with an APIVersion bump.
package runrecord

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/buildinfo"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
)

const (
	APIVersion  = "jin/v1alpha1"
	KindPlanRun = "PlanRun"
)

type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Tool struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Cluster deliberately omits the API server endpoint and credentials.
type Cluster struct {
	Context      string       `json:"context"`
	Provider     string       `json:"provider,omitempty"`
	Version      kube.Version `json:"version"`
	GitVersion   string       `json:"gitVersion,omitempty"`
	Nodes        int          `json:"nodes"`
	HelmReleases int          `json:"helmReleases"`
}

type Record struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Tool       Tool       `json:"tool"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt time.Time  `json:"finishedAt"`
	Status     Status     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Cluster    Cluster    `json:"cluster"`
	Plan       *plan.Plan `json:"plan,omitempty"`
}

var idPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{6}$`)

func New(kind string, now time.Time) *Record {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	now = now.UTC()
	return &Record{
		APIVersion: APIVersion,
		Kind:       kind,
		ID:         now.Format("20060102T150405Z") + "-" + hex.EncodeToString(b),
		Tool:       Tool{Version: buildinfo.Version, Commit: buildinfo.Commit},
		StartedAt:  now,
	}
}

func (r *Record) Finish(now time.Time, err error) {
	r.FinishedAt = now.UTC()
	if err != nil {
		r.Status = StatusFailed
		r.Error = err.Error()
		return
	}
	r.Status = StatusSucceeded
}

type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

// DefaultDir is $JIN_HOME/runs, falling back to ~/.jin/runs.
func DefaultDir() (string, error) {
	if d := os.Getenv("JIN_HOME"); d != "" {
		return filepath.Join(d, "runs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".jin", "runs"), nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Save(r *Record) error {
	if !idPattern.MatchString(r.ID) {
		return fmt.Errorf("invalid run id %q", r.ID)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*.json")
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
	return os.Rename(tmp.Name(), filepath.Join(s.dir, r.ID+".json"))
}

func (s *Store) Get(id string) (*Record, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("run %s not found in %s", id, s.dir)
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode run %s: %w", id, err)
	}
	return &r, nil
}

// List returns records newest first. Unreadable files are skipped.
func (s *Store) List() ([]*Record, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		r, err := s.Get(id)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}
