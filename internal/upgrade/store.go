package upgrade

import (
	"bufio"
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
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")

	idPattern = regexp.MustCompile(`^up-\d{8}T\d{6}Z-[0-9a-f]{6}$`)
)

func newID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "up-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// Store persists upgrades as JSON documents and their events as append-only JSON lines.
// It is not safe for use by more than one process.
type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(id, ext string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid upgrade id %q: %w", id, ErrNotFound)
	}
	return filepath.Join(s.dir, id+ext), nil
}

func (s *Store) Save(u *Upgrade) error {
	p, err := s.path(u.ID, ".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(u, "", "  ")
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
	return os.Rename(tmp.Name(), p)
}

func (s *Store) Get(id string) (*Upgrade, error) {
	p, err := s.path(id, ".json")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("upgrade %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var u Upgrade
	if err := json.Unmarshal(b, &u); err != nil {
		return nil, fmt.Errorf("decode upgrade %s: %w", id, err)
	}
	return &u, nil
}

// List returns upgrades newest first.
func (s *Store) List() ([]*Upgrade, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Upgrade
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		if u, err := s.Get(id); err == nil {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) AppendEvent(id string, ev Event) error {
	p, err := s.path(id, ".events.jsonl")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// Events returns events with Seq greater than after, in order.
func (s *Store) Events(id string, after int64) ([]Event, error) {
	p, err := s.path(id, ".events.jsonl")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}
