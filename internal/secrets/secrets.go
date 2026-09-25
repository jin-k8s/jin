// Package secrets stores credentials entered in the UI, encrypted at rest with AES-256-GCM.
//
// The key lives in a separate 0600 file, so a leaked secrets file or backup alone reveals nothing.
// It does not protect against an attacker who can read the whole $JIN_HOME; teams that need that
// should keep supplying credentials through the environment or a secret manager instead.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrNotFound = errors.New("secret not found")

type entry struct {
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
	Hint       string    `json:"hint"`
	SetAt      time.Time `json:"setAt"`
	SetBy      string    `json:"setBy"`
	// Meta holds non-secret facts about the value (e.g. the GitHub login it belongs to).
	Meta map[string]string `json:"meta,omitempty"`
}

// Info describes a stored secret without revealing it.
type Info struct {
	Hint  string            `json:"hint"`
	SetAt time.Time         `json:"setAt"`
	SetBy string            `json:"setBy"`
	Meta  map[string]string `json:"meta,omitempty"`
}

type Store struct {
	path string
	aead cipher.AEAD
	mu   sync.Mutex
}

// Open loads or creates the key at keyPath and the store at path.
func Open(path, keyPath string) (*Store, error) {
	key, err := loadKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, aead: aead}, nil
}

func loadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(string(trim(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s is not a valid 256-bit key", path)
		}
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600)
}

func trim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func (s *Store) load() (map[string]entry, error) {
	m := map[string]entry{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func (s *Store) save(m map[string]entry) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*.json")
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

func hint(v string) string {
	if len(v) <= 8 {
		return "…"
	}
	return "…" + v[len(v)-4:]
}

// Put encrypts value under name; the name is bound into the ciphertext so entries cannot be swapped.
func (s *Store) Put(name, value, by string, meta map[string]string) error {
	if value == "" {
		return errors.New("empty secret")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[name] = entry{Nonce: nonce, Ciphertext: s.aead.Seal(nil, nonce, []byte(value), []byte(name)), Hint: hint(value), SetAt: time.Now().UTC(), SetBy: by, Meta: meta}
	return s.save(m)
}

func (s *Store) Get(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return "", err
	}
	e, ok := m[name]
	if !ok {
		return "", ErrNotFound
	}
	pt, err := s.aead.Open(nil, e.Nonce, e.Ciphertext, []byte(name))
	if err != nil {
		return "", fmt.Errorf("decrypt %s: %w", name, err)
	}
	return string(pt), nil
}

func (s *Store) Info(name string) (*Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	e, ok := m[name]
	if !ok {
		return nil, ErrNotFound
	}
	return &Info{Hint: e.Hint, SetAt: e.SetAt, SetBy: e.SetBy, Meta: e.Meta}, nil
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.save(m)
}
