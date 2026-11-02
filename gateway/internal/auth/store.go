// Package auth manages local, user-linked API keys. The server verifies only
// hashes; reusable client secrets live in a separate credentials file.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidKey = errors.New("invalid API key")

type Principal struct {
	KeyID  string
	UserID string
}
type Record struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Hash      string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}
type keyFile struct {
	Version int      `json:"version"`
	Keys    []Record `json:"keys"`
}
type SavedKey struct {
	UserID  string `json:"user_id"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}
type credentials struct {
	Version     int                 `json:"version"`
	ActiveKeyID string              `json:"active_key_id"`
	Keys        map[string]SavedKey `json:"keys"`
}
type Store struct{ Dir string }

func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "llm-gateway-scheduler"), nil
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("key directory is required")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir}
	if err := s.checkDir(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) checkDir() error {
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("key directory must be a real directory accessible only to its owner (chmod 700): %s", s.Dir)
	}
	return nil
}
func (s *Store) CredentialsPath() string { return filepath.Join(s.Dir, "credentials.json") }
func (s *Store) serverPath() string      { return filepath.Join(s.Dir, "api-keys.json") }

func secureFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("key file must be a regular file accessible only to its owner (chmod 600): %s", path)
	}
	return nil
}
func readJSON(path string, value any) error {
	if err := secureFile(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 1<<20 {
		return errors.New("key file exceeds 1 MiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid key file JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid trailing key file data")
	}
	return nil
}
func writeJSON(path string, value any) error {
	if err := secureFile(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > 1<<20 {
		return errors.New("key file exceeds 1 MiB")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
func validUser(user string) bool {
	if len(user) == 0 || len(user) > 64 {
		return false
	}
	for _, c := range user {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@-", c)) {
			return false
		}
	}
	return true
}
func (s *Store) loadKeys() (keyFile, error) {
	result := keyFile{Version: 1, Keys: []Record{}}
	if err := s.checkDir(); err != nil {
		return result, err
	}
	if err := readJSON(s.serverPath(), &result); err != nil {
		return result, err
	}
	if result.Version != 1 || result.Keys == nil {
		return result, errors.New("invalid key-store format")
	}
	seen := map[string]bool{}
	for _, record := range result.Keys {
		id, idErr := hex.DecodeString(record.ID)
		hash, hashErr := hex.DecodeString(record.Hash)
		if idErr != nil || len(id) != 8 || hashErr != nil || len(hash) != sha256.Size || !validUser(record.UserID) || seen[record.ID] {
			return result, errors.New("invalid key-store record")
		}
		seen[record.ID] = true
	}
	return result, nil
}
func (s *Store) loadCredentials() (credentials, error) {
	result := credentials{Version: 1, Keys: map[string]SavedKey{}}
	if err := s.checkDir(); err != nil {
		return result, err
	}
	if err := readJSON(s.CredentialsPath(), &result); err != nil {
		return result, err
	}
	if result.Version != 1 || result.Keys == nil {
		return result, errors.New("invalid credentials format")
	}
	return result, nil
}
func localURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("base URL must be a local HTTP origin without a path or credentials")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return "", errors.New("base URL must use a loopback host")
	}
	port, portErr := strconv.Atoi(u.Port())
	if portErr != nil || port < 1 || port > 65535 {
		return "", errors.New("base URL must include a valid gateway port")
	}
	u.Path = ""
	return u.String(), nil
}

// Create saves the hash first, then the client copy, under a cross-process lock.
// A failed client write rolls back registration; the CLI can display the saved key.
func (s *Store) Create(user, baseURL string) (Record, error) {
	var record Record
	if !validUser(user) {
		return record, errors.New("user must be 1-64 letters, digits, dots, underscores, @ or hyphens")
	}
	baseURL, err := localURL(baseURL)
	if err != nil {
		return record, err
	}
	unlock, err := s.lock()
	if err != nil {
		return record, err
	}
	defer unlock()
	keys, err := s.loadKeys()
	if err != nil {
		return record, err
	}
	saved, err := s.loadCredentials()
	if err != nil {
		return record, err
	}
	random := make([]byte, 40)
	if _, err := rand.Read(random); err != nil {
		return record, err
	}
	id := hex.EncodeToString(random[:8])
	for _, existing := range keys.Keys {
		if existing.ID == id {
			return record, errors.New("key ID collision; retry creation")
		}
	}
	token := "lgw_" + id + "_" + base64.RawURLEncoding.EncodeToString(random[8:])
	hash := sha256.Sum256([]byte(token))
	record = Record{ID: id, UserID: user, Hash: hex.EncodeToString(hash[:]), CreatedAt: time.Now().UTC()}
	keys.Keys = append(keys.Keys, record)
	if err := writeJSON(s.serverPath(), keys); err != nil {
		return Record{}, err
	}
	saved.Keys[id] = SavedKey{UserID: user, APIKey: token, BaseURL: baseURL}
	saved.ActiveKeyID = id
	if err := writeJSON(s.CredentialsPath(), saved); err != nil {
		keys.Keys = keys.Keys[:len(keys.Keys)-1]
		rollbackErr := writeJSON(s.serverPath(), keys)
		return Record{}, errors.Join(errors.New("could not save client credentials"), err, rollbackErr)
	}
	return record, nil
}

// Verify reads the atomically replaced hash file on every request, so revocation
// affects the next request without restarting the gateway.
func (s *Store) Verify(token string) (Principal, error) {
	if len(token) != 64 || !strings.HasPrefix(token, "lgw_") || token[20] != '_' {
		return Principal{}, ErrInvalidKey
	}
	id := token[4:20]
	keys, err := s.loadKeys()
	if err != nil {
		return Principal{}, err
	}
	digest := sha256.Sum256([]byte(token))
	for _, record := range keys.Keys {
		if record.ID != id {
			continue
		}
		expected, _ := hex.DecodeString(record.Hash)
		if subtle.ConstantTimeCompare(expected, digest[:]) != 1 || record.Revoked {
			return Principal{}, ErrInvalidKey
		}
		return Principal{KeyID: record.ID, UserID: record.UserID}, nil
	}
	return Principal{}, ErrInvalidKey
}
func (s *Store) List() ([]Record, error) {
	keys, err := s.loadKeys()
	return keys.Keys, err
}
func (s *Store) Revoke(id string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	keys, err := s.loadKeys()
	if err != nil {
		return err
	}
	for i := range keys.Keys {
		if keys.Keys[i].ID == id {
			keys.Keys[i].Revoked = true
			return writeJSON(s.serverPath(), keys)
		}
	}
	return errors.New("key ID not found")
}
func (s *Store) Show(id string) (string, error) {
	saved, err := s.loadCredentials()
	if err != nil {
		return "", err
	}
	if id == "" {
		id = saved.ActiveKeyID
	}
	key, ok := saved.Keys[id]
	if !ok {
		return "", errors.New("no saved key selected; create a key first")
	}
	if _, err := s.Verify(key.APIKey); err != nil {
		return "", err
	}
	return key.APIKey, nil
}
func (s *Store) Use(id string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	saved, err := s.loadCredentials()
	if err != nil {
		return err
	}
	key, ok := saved.Keys[id]
	if !ok {
		return errors.New("saved key ID not found")
	}
	if _, err := s.Verify(key.APIKey); err != nil {
		return err
	}
	saved.ActiveKeyID = id
	return writeJSON(s.CredentialsPath(), saved)
}
