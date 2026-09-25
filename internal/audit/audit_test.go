package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChainAndTamperDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"login", "upgrade.approve", "upgrade.cancel"} {
		if err := l.Record(Entry{Actor: "alice@x", Action: a, Target: "=HYPERLINK(\"evil\")"}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _, err := l.Verify(); !ok || err != nil {
		t.Fatalf("fresh log must verify: %v", err)
	}

	l2, _ := Open(path)
	_ = l2.Record(Entry{Actor: "bob@x", Action: "logout"})
	es, _ := l2.Entries(time.Time{})
	if len(es) != 4 || es[3].Seq != 4 || es[3].Prev != es[2].Hash {
		t.Fatalf("chain must continue after reopen: %+v", es[3])
	}

	var buf bytes.Buffer
	_ = WriteCSV(&buf, es)
	if !strings.Contains(buf.String(), `'=HYPERLINK`) {
		t.Fatalf("formula injection not neutralised:\n%s", buf.String())
	}

	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, bytes.Replace(b, []byte("upgrade.cancel"), []byte("upgrade.retry"), 1), 0o600)
	if ok, at, _ := l2.Verify(); ok || at != 3 {
		t.Fatalf("tampering must be detected at entry 3: ok=%v at=%d", ok, at)
	}
}
