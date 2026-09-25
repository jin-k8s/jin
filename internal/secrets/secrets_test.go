package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripAndAtRestEncryption(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "secrets.json"), filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	const tok = "github_pat_11ABCDEFG0123456789_supersecretvalue"
	if err := s.Put("github", tok, "alice", map[string]string{"login": "alice"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("github"); err != nil || got != tok {
		t.Fatalf("get: %q %v", got, err)
	}
	info, _ := s.Info("github")
	if info.Hint != "…alue" || info.Meta["login"] != "alice" {
		t.Fatalf("info: %+v", info)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if bytes.Contains(raw, []byte("supersecret")) {
		t.Fatal("plaintext on disk")
	}
	for _, f := range []string{"secrets.json", "secrets.key"} {
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms %v", f, st.Mode().Perm())
		}
	}

	// Reopening with the same key works; a different key cannot decrypt.
	s2, _ := Open(filepath.Join(dir, "secrets.json"), filepath.Join(dir, "secrets.key"))
	if got, _ := s2.Get("github"); got != tok {
		t.Fatal("reopen failed")
	}
	s3, _ := Open(filepath.Join(dir, "secrets.json"), filepath.Join(t.TempDir(), "other.key"))
	if _, err := s3.Get("github"); err == nil {
		t.Fatal("wrong key must not decrypt")
	}

	// Entries are bound to their name: moving ciphertext to another name fails.
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	m["other"] = m["github"]
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "secrets.json"), b, 0o600)
	if _, err := s.Get("other"); err == nil {
		t.Fatal("swapped ciphertext must not decrypt")
	}

	_ = s.Delete("github")
	if _, err := s.Get("github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
}
