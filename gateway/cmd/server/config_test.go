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

func TestCombinedConfigurationReload(testContext *testing.T) {
	path := filepath.Join(testContext.TempDir(), "gateway.json")
	config := registry.Default()
	write := func() {
		testContext.Helper()
		raw, err := json.Marshal(config)
		if err != nil {
			testContext.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			testContext.Fatal(err)
		}
	}
	write()
	handler, listen, reload, err := configureHandler(configVerifier{}, path, "", "")
	if err != nil || listen != "127.0.0.1:8000" {
		testContext.Fatal(listen, err)
	}
	list := func() string {
		request := httptest.NewRequest("GET", "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer test")
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, request)
		if responseRecorder.Code != 200 {
			testContext.Fatal(responseRecorder.Code)
		}
		return responseRecorder.Body.String()
	}
	if !strings.Contains(list(), "local-llm") {
		testContext.Fatal("initial model missing")
	}
	second := config.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	config.Models = append(config.Models, second)
	write()
	if strings.Contains(list(), "second") {
		testContext.Fatal("config modified without reload")
	}
	if err := reload(); err != nil {
		testContext.Fatal(err)
	}
	if !strings.Contains(list(), "second") {
		testContext.Fatal("registration reload failed")
	}
	os.WriteFile(path, []byte("{"), 0600)
	if err := reload(); err == nil || !strings.Contains(list(), "second") {
		testContext.Fatal("bad reload replaced active config")
	}
	if _, _, _, err := configureHandler(configVerifier{}, path, path, ""); err == nil {
		testContext.Fatal("conflicting flags accepted")
	}
}
