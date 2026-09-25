package runrecord

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
)

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "runs"))

	older := New(KindPlanRun, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	older.Finish(older.StartedAt, errors.New("boom"))
	newer := New(KindPlanRun, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	newer.Plan = &plan.Plan{Current: kube.MustParseVersion("1.30"), Target: kube.MustParseVersion("1.31"), Ready: true}
	newer.Finish(newer.StartedAt, nil)

	for _, r := range []*Record{older, newer} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Stat(filepath.Join(s.Dir(), newer.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record perms = %v, want 0600", info.Mode().Perm())
	}

	got, err := s.Get(newer.ID)
	if err != nil || got.Plan == nil || got.Plan.Target != kube.MustParseVersion("1.31") || got.Status != StatusSucceeded {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].ID != newer.ID || list[1].Error != "boom" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestGetRejectsPathTraversal(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, id := range []string{"../../etc/passwd", "", "20260901T000000Z-abc"} {
		if _, err := s.Get(id); err == nil {
			t.Errorf("Get(%q) should fail", id)
		}
	}
}

func TestListMissingDir(t *testing.T) {
	recs, err := NewStore(filepath.Join(t.TempDir(), "nope")).List()
	if err != nil || len(recs) != 0 {
		t.Fatalf("got %v, %v", recs, err)
	}
}
