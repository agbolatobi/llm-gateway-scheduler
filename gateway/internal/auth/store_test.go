//go:build linux

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestStore(testContext *testing.T) *Store {
	testContext.Helper()
	store, err := Open(filepath.Join(testContext.TempDir(), "keys"))
	if err != nil {
		testContext.Fatal(err)
	}
	return store
}
func TestCreatePersistRevokeAndReuse(testContext *testing.T) {
	store := newTestStore(testContext)
	first, err := store.Create("alice", "http://127.0.0.1:8000")
	if err != nil {
		testContext.Fatal(err)
	}
	token, err := store.Show("")
	if err != nil {
		testContext.Fatal(err)
	}
	if len(token) != 64 {
		testContext.Fatal("unexpected key length")
	}
	data, err := os.ReadFile(store.serverPath())
	if err != nil {
		testContext.Fatal(err)
	}
	if strings.Contains(string(data), token) || !strings.Contains(string(data), first.Hash) {
		testContext.Fatal("server must contain hashes only")
	}
	for _, path := range []string{store.serverPath(), store.CredentialsPath()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			testContext.Fatal("key files must have mode 0600")
		}
	}
	reopened, err := Open(store.Dir)
	if err != nil {
		testContext.Fatal(err)
	}
	principal, err := reopened.Verify(token)
	if err != nil || principal.UserID != "alice" || principal.KeyID != first.ID {
		testContext.Fatal("saved key failed verification")
	}
	second, err := reopened.Create("alice", "http://localhost:8000")
	if err != nil || second.ID == first.ID || second.Hash == first.Hash {
		testContext.Fatal("keys must be unique")
	}
	if err := reopened.Use(first.ID); err != nil {
		testContext.Fatal(err)
	}
	selected, err := reopened.Show("")
	if err != nil || selected != token {
		testContext.Fatal("key selection was not persisted")
	}
	if err := reopened.Revoke(first.ID); err != nil {
		testContext.Fatal(err)
	}
	if _, err := store.Verify(token); !errors.Is(err, ErrInvalidKey) {
		testContext.Fatal("revocation must apply to existing store instances")
	}
	if _, err := reopened.Show(first.ID); !errors.Is(err, ErrInvalidKey) {
		testContext.Fatal("revoked keys must not be selected for reuse")
	}
	secondToken, err := reopened.Show(second.ID)
	if err != nil {
		testContext.Fatal(err)
	}
	if principal, err := reopened.Verify(secondToken); err != nil || principal.UserID != "alice" {
		testContext.Fatal("revoking one key must not revoke another")
	}
	if err := reopened.Revoke(first.ID); err != nil {
		testContext.Fatal("revoke should be idempotent")
	}
	if err := reopened.Revoke("missing"); err == nil {
		testContext.Fatal("unknown key should fail")
	}
}

func TestInvalidKeysAndCorruptStorage(testContext *testing.T) {
	store := newTestStore(testContext)
	if _, err := store.Create("alice", "http://127.0.0.1:8000"); err != nil {
		testContext.Fatal(err)
	}
	token, _ := store.Show("")
	for _, invalid := range []string{"", "wrong", token[:63], token + "x", token[:63] + "!"} {
		if _, err := store.Verify(invalid); !errors.Is(err, ErrInvalidKey) {
			testContext.Fatal("invalid key was not rejected")
		}
	}
	if err := os.WriteFile(store.serverPath(), []byte("{"), 0600); err != nil {
		testContext.Fatal(err)
	}
	if _, err := store.Verify(token); err == nil || errors.Is(err, ErrInvalidKey) {
		testContext.Fatal("corrupt store must fail closed with a storage error")
	}
}

func TestUnsafeFilesAreRejected(testContext *testing.T) {
	store := newTestStore(testContext)
	if _, err := store.Create("alice", "http://127.0.0.1:8000"); err != nil {
		testContext.Fatal(err)
	}
	token, _ := store.Show("")
	if err := os.Chmod(store.serverPath(), 0644); err != nil {
		testContext.Fatal(err)
	}
	if _, err := store.Verify(token); err == nil {
		testContext.Fatal("world-readable store should be rejected")
	}
	if err := os.Chmod(store.serverPath(), 0600); err != nil {
		testContext.Fatal(err)
	}
	if err := os.Chmod(store.Dir, 0755); err != nil {
		testContext.Fatal(err)
	}
	if _, err := store.Verify(token); err == nil {
		testContext.Fatal("insecure directory should be rejected")
	}
	if err := os.Chmod(store.Dir, 0700); err != nil {
		testContext.Fatal(err)
	}
	other := newTestStore(testContext)
	if err := os.Symlink(store.CredentialsPath(), other.CredentialsPath()); err != nil {
		testContext.Fatal(err)
	}
	if _, err := other.Create("bob", "http://127.0.0.1:8000"); err == nil {
		testContext.Fatal("symlink credentials should be rejected")
	}
}

func TestConcurrentAdminDoesNotLoseKeys(testContext *testing.T) {
	store := newTestStore(testContext)
	const count = 12
	failures := make(chan error, count)
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.Create("alice", "http://127.0.0.1:8000")
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			testContext.Fatal(err)
		}
	}
	records, err := store.List()
	if err != nil || len(records) != count {
		testContext.Fatal("concurrent key creation lost updates")
	}
	for _, record := range records {
		token, err := store.Show(record.ID)
		if err != nil {
			testContext.Fatal(err)
		}
		if _, err := store.Verify(token); err != nil {
			testContext.Fatal(err)
		}
	}
}

func TestCreationValidationAndClientWriteRollback(testContext *testing.T) {
	store := newTestStore(testContext)
	for _, user := range []string{"", "has space", "line\nbreak", strings.Repeat("a", 65)} {
		if _, err := store.Create(user, "http://127.0.0.1:8000"); err == nil {
			testContext.Fatal("invalid user accepted")
		}
	}
	for _, address := range []string{"http://example.com:8000", "http://localhost", "http://localhost:8000/v1", "http://user@localhost:8000", "http://localhost:0", "http://localhost:99999"} {
		if _, err := store.Create("alice", address); err == nil {
			testContext.Fatal("invalid origin accepted")
		}
	}
	// An oversized credentials update must roll back its newly registered hash.
	saved := credentials{Version: 1, Keys: map[string]SavedKey{
		"large": {UserID: "alice", APIKey: strings.Repeat("x", (1<<20)-220), BaseURL: "http://127.0.0.1:8000"},
	}}
	if err := writeJSON(store.CredentialsPath(), saved); err != nil {
		testContext.Fatal(err)
	}
	if _, err := store.Create("alice", "http://127.0.0.1:8000"); err == nil {
		testContext.Fatal("oversized client file should fail")
	}
	records, err := store.List()
	if err != nil || len(records) != 0 {
		testContext.Fatal("failed save left a registered key")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, ".keys.lock")); err != nil {
		testContext.Fatal(err)
	}
}
