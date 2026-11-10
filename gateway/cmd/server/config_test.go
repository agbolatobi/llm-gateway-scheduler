package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/registry"
)

type configVerifier struct{}

func (configVerifier) Verify(string) (auth.Principal, error) {
	return auth.Principal{UserID: "alice", KeyID: "test"}, nil
}

func TestCombinedConfigurationReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.json")
	c := registry.Default()
	write := func() {
		t.Helper()
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	h, listen, reload, err := configureHandler(configVerifier{}, path, "", "")
	if err != nil || listen != "127.0.0.1:8000" {
		t.Fatal(listen, err)
	}
	list := func() string {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		return w.Body.String()
	}
	if !strings.Contains(list(), "local-llm") {
		t.Fatal("initial model missing")
	}
	second := c.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	c.Models = append(c.Models, second)
	write()
	if strings.Contains(list(), "second") {
		t.Fatal("config modified without reload")
	}
	if err := reload(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list(), "second") {
		t.Fatal("registration reload failed")
	}
	os.WriteFile(path, []byte("{"), 0600)
	if err := reload(); err == nil || !strings.Contains(list(), "second") {
		t.Fatal("bad reload replaced active config")
	}
	if _, _, _, err := configureHandler(configVerifier{}, path, path, ""); err == nil {
		t.Fatal("conflicting flags accepted")
	}
}
