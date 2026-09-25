// Package audit keeps a hash-chained, append-only log of security-relevant actions.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Role   string    `json:"role,omitempty"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Remote string    `json:"remote,omitempty"`
	// Prev and Hash chain entries: editing or deleting a line breaks every later hash.
	Prev string `json:"prev"`
	Hash string `json:"hash"`
}

func (e Entry) digest() string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Log struct {
	path string
	mu   sync.Mutex
	seq  int64
	last string
	now  func() time.Time
}

func Open(path string) (*Log, error) {
	l := &Log{path: path, now: time.Now}
	entries, err := l.read()
	if err != nil {
		return nil, err
	}
	if n := len(entries); n > 0 {
		l.seq, l.last = entries[n-1].Seq, entries[n-1].Hash
	}
	return l, nil
}

func (l *Log) Record(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq, e.Time, e.Prev = l.seq, l.now().UTC(), l.last
	e.Hash = e.digest()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return errors.Join(err, f.Close())
	}
	l.last = e.Hash
	return f.Close()
}

func (l *Log) read() ([]Entry, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("audit log line %d: %w", len(out)+1, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Entries returns entries at or after since.
func (l *Log) Entries(since time.Time) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all, err := l.read()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if !e.Time.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Verify checks the hash chain; it returns the first broken sequence number.
func (l *Log) Verify() (ok bool, brokenAt int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all, err := l.read()
	if err != nil {
		return false, 0, err
	}
	prev := ""
	for _, e := range all {
		if e.Prev != prev || e.Hash != e.digest() {
			return false, e.Seq, nil
		}
		prev = e.Hash
	}
	return true, 0, nil
}

func WriteCSV(w io.Writer, entries []Entry) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"seq", "time", "actor", "role", "action", "target", "detail", "remote", "hash"})
	for _, e := range entries {
		_ = cw.Write([]string{fmt.Sprint(e.Seq), e.Time.Format(time.RFC3339), csvSafe(e.Actor), csvSafe(e.Role), csvSafe(e.Action),
			csvSafe(e.Target), csvSafe(e.Detail), csvSafe(e.Remote), e.Hash})
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe neutralises values that spreadsheets would evaluate as formulas.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
