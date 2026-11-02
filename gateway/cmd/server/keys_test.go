//go:build linux

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/auth"
)

func TestKeyCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	var out, errOut bytes.Buffer
	run := func(args ...string) error {
		out.Reset()
		errOut.Reset()
		args = append(args, "--config-dir", dir)
		return runKeys(args, &out, &errOut)
	}
	if err := run("create", "--user", "alice"); err != nil {
		t.Fatal(err)
	}
	store, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.Show("")
	if err != nil {
		t.Fatal(err)
	}
	records, _ := store.List()
	if !strings.Contains(out.String(), "API token: "+token+"\n") || strings.Count(out.String(), token) != 1 || strings.Contains(out.String(), records[0].Hash) {
		t.Fatal("create must print the complete saved token exactly once, without the verification hash")
	}
	if err := run("list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice") || strings.Contains(out.String(), token) || strings.Contains(out.String(), records[0].Hash) {
		t.Fatal("list must show metadata only")
	}
	if err := run("show"); err != nil || strings.TrimSpace(out.String()) != token {
		t.Fatal("explicit show should return the saved key")
	}
	if err := run("use", "--id", records[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := run("revoke", "--id", records[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := run("show"); err == nil {
		t.Fatal("show must reject a revoked key")
	}
	if err := run("revoke"); err == nil {
		t.Fatal("revoke requires ID")
	}
	if err := run("create"); err == nil {
		t.Fatal("create requires user")
	}
}
