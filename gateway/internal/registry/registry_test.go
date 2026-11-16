package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfigAliasesAndClone(testContext *testing.T) {
	config := Default()
	if err := config.Validate(); err != nil {
		testContext.Fatal(err)
	}
	model, ok := config.Lookup("default")
	if !ok || model.ID != "local-llm" {
		testContext.Fatal("alias resolution")
	}
	copy := config.Clone()
	copy.Models[0].Aliases[0] = "changed"
	if config.Models[0].Aliases[0] != "default" {
		testContext.Fatal("clone shares aliases")
	}
	config.Models = append(config.Models, config.Models[0])
	if err := config.Validate(); err == nil {
		testContext.Fatal("duplicate accepted")
	}
}

func TestRemovedModelPermissionsRejected(testContext *testing.T) {
	// Reject obsolete restrictions rather than silently ignoring operator intent.
	for _, users := range []string{`null`, `[]`, `["alice"]`} {
		raw, _ := json.Marshal(Default())
		body := strings.Replace(string(raw), `"models":[{`, `"models":[{"allowed_users":`+users+`,`, 1)
		path := filepath.Join(testContext.TempDir(), "gateway.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			testContext.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			testContext.Fatal("obsolete allowed_users accepted")
		}
	}
}

func TestStrictFileAndInvalidConfig(testContext *testing.T) {
	path := filepath.Join(testContext.TempDir(), "gateway.json")
	for _, body := range []string{`null`, `{}`, `{"version":1,"version":1}`, `{"Version":1}`, `[]`} {
		os.WriteFile(path, []byte(body), 0600)
		if _, err := Load(path); err == nil {
			testContext.Fatal("invalid file accepted")
		}
	}
	config := Default()
	raw, _ := json.Marshal(config)
	os.WriteFile(path, raw, 0600)
	if _, err := Load(path); err != nil {
		testContext.Fatal(err)
	}
	os.WriteFile(path, append(raw, []byte(` {}`)...), 0600)
	if _, err := Load(path); err == nil {
		testContext.Fatal("trailing JSON accepted")
	}
	for _, change := range []func(*Config){
		func(config *Config) { config.Version = 2 }, func(config *Config) { config.Models[0].MaxOutputTokens = config.Models[0].ContextLimit },
		func(config *Config) { config.Models[0].Backend.BaseURL = "http://example.com:8080" },
		func(config *Config) { config.Models[0].Backend.BaseURL = "http://127.0.0.1:8000" },
		func(config *Config) { config.Models[0].Backend.Engine = "unknown" },
		func(config *Config) { config.Models[0].Aliases = []string{"local-llm"} },
		func(config *Config) { config.Models[0].Launch = &Launch{} },
	} {
		next := Default()
		change(&next)
		if err := next.Validate(); err == nil {
			testContext.Fatal("invalid config accepted")
		}
	}
	for _, raw := range []string{"http://127.0.0.1", "http://user:pass@127.0.0.1:8080", "http://127.0.0.1:8080/v1", "http://127.0.0.1:8080?x=1", "https://127.0.0.1:8080"} {
		if _, err := LocalURL(raw); err == nil {
			testContext.Fatal("unsafe URL accepted", raw)
		}
	}
}

func TestCompatibleReload(testContext *testing.T) {
	config := Default()
	next := config.Clone()
	next.Models[0].Limits.Requests++
	if err := config.CompatibleReload(next); err != nil {
		testContext.Fatal(err)
	}
	next.Models[0].Backend.BaseURL = "http://127.0.0.1:8082"
	if err := config.CompatibleReload(next); err == nil {
		testContext.Fatal("runtime mutation accepted")
	}
}

func TestRuntimeTemplateAndIdentity(testContext *testing.T) {
	for _, scenario := range []string{"valid", "identity", "context", "bad tokens", "redirect"} {
		testContext.Run(scenario, func(testContext *testing.T) {
			var generation atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/health":
					if scenario == "redirect" {
						http.Redirect(responseWriter, request, "/v1/chat/completions", 302)
						return
					}
					responseWriter.Write([]byte(`{"status":"ok"}`))
				case "/v1/models":
					name := "local-llm"
					if scenario == "identity" {
						name = "wrong"
					}
					json.NewEncoder(responseWriter).Encode(map[string]any{"data": []map[string]string{{"id": name}}})
				case "/slots":
					count := 4096
					if scenario == "context" {
						count = 512
					}
					json.NewEncoder(responseWriter).Encode([]map[string]int{{"n_ctx": count}})
				case "/apply-template":
					var body map[string]any
					json.NewDecoder(request.Body).Decode(&body)
					if body["messages"] == nil {
						testContext.Error("messages missing")
					}
					responseWriter.Write([]byte(`{"prompt":"<chat>hello</chat>"}`))
				case "/tokenize":
					var body map[string]any
					json.NewDecoder(request.Body).Decode(&body)
					if body["content"] != "<chat>hello</chat>" || body["add_special"] != true || body["parse_special"] != true {
						testContext.Error("template or specials missing")
					}
					if scenario == "bad tokens" {
						responseWriter.Write([]byte(`{"tokens":[null]}`))
					} else {
						responseWriter.Write([]byte(`{"tokens":[1,2,3,4,5]}`))
					}
				default:
					generation.Add(1)
					http.Error(responseWriter, "unexpected inference", 500)
				}
			}))
			defer backend.Close()
			model := Default().Models[0]
			model.Backend.BaseURL = backend.URL
			count, err := NewInspector().CountInput(context.Background(), model, []map[string]string{{"role": "user", "content": "hello"}})
			if scenario == "valid" {
				if err != nil || count != 5 {
					testContext.Fatal(count, err)
				}
			} else if err == nil {
				testContext.Fatal("invalid runtime accepted")
			}
			if generation.Load() != 0 {
				testContext.Fatal("generation or redirect reached")
			}
		})
	}
}

func TestInspectorCancellation(testContext *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewInspector().CountInput(ctx, Default().Models[0], nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		testContext.Fatal(err)
	}
}
