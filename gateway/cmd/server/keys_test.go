//go:build linux

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/auth"
)

func TestKeyCommands(testContext *testing.T) {
	dir := filepath.Join(testContext.TempDir(), "keys")
	var out, errOut bytes.Buffer
	run := func(args ...string) error {
		out.Reset()
		errOut.Reset()
		args = append(args, "--config-dir", dir)
		return runKeys(args, &out, &errOut)
	}
	if err := run("create", "--user", "alice"); err != nil {
		testContext.Fatal(err)
	}
	store, err := auth.Open(dir)
	if err != nil {
		testContext.Fatal(err)
	}
	token, err := store.Show("")
	if err != nil {
		testContext.Fatal(err)
	}
	records, _ := store.List()
	if !strings.Contains(out.String(), "API token: "+token+"\n") || strings.Count(out.String(), token) != 1 || strings.Contains(out.String(), records[0].Hash) {
		testContext.Fatal("create must print the complete saved token exactly once, without the verification hash")
	}
	if !strings.Contains(out.String(), "Chat UI: http://127.0.0.1:8000/chat/\n") {
		testContext.Fatal("create must print the local chat URL without a token")
	}
	if err := run("list"); err != nil {
		testContext.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice") || strings.Contains(out.String(), token) || strings.Contains(out.String(), records[0].Hash) {
		testContext.Fatal("list must show metadata only")
	}
	if err := run("show"); err != nil || strings.TrimSpace(out.String()) != token {
		testContext.Fatal("explicit show should return the saved key")
	}
	if err := run("use", "--id", records[0].ID); err != nil {
		testContext.Fatal(err)
	}
	if err := run("revoke", "--id", records[0].ID); err != nil {
		testContext.Fatal(err)
	}
	if err := run("show"); err == nil {
		testContext.Fatal("show must reject a revoked key")
	}
	if err := run("revoke"); err == nil {
		testContext.Fatal("revoke requires ID")
	}
	if err := run("create"); err == nil {
		testContext.Fatal("create requires user")
	}
}

func TestKeyCreationPublicChatLink(testContext *testing.T) {
	directory := filepath.Join(testContext.TempDir(), "keys")
	var output, errorsOutput bytes.Buffer
	arguments := []string{"create", "--user", "alice", "--config-dir", directory, "--chat-url", "https://chat.example.com"}
	if err := runKeys(arguments, &output, &errorsOutput); err != nil {
		testContext.Fatal(err)
	}
	if !strings.Contains(output.String(), "Chat UI: https://chat.example.com/chat/\n") {
		testContext.Fatal("missing public chat URL")
	}
	store, err := auth.Open(directory)
	if err != nil {
		testContext.Fatal(err)
	}
	arguments[len(arguments)-1] = "http://public.example.com"
	if err := runKeys(arguments, &output, &errorsOutput); err == nil {
		testContext.Fatal("unsafe public URL accepted")
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		testContext.Fatal("invalid URL must be rejected before generating a key")
	}
}
