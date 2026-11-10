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

func TestConfigAliasesAndClone(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	m, ok := c.Lookup("default")
	if !ok || m.ID != "local-llm" {
		t.Fatal("alias resolution")
	}
	copy := c.Clone()
	copy.Models[0].Aliases[0] = "changed"
	if c.Models[0].Aliases[0] != "default" {
		t.Fatal("clone shares aliases")
	}
	c.Models = append(c.Models, c.Models[0])
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestRemovedModelPermissionsRejected(t *testing.T) {
	// Reject obsolete restrictions rather than silently ignoring operator intent.
	for _, users := range []string{`null`, `[]`, `["alice"]`} {
		raw, _ := json.Marshal(Default())
		body := strings.Replace(string(raw), `"models":[{`, `"models":[{"allowed_users":`+users+`,`, 1)
		path := filepath.Join(t.TempDir(), "gateway.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("obsolete allowed_users accepted")
		}
	}
}

func TestStrictFileAndInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.json")
	for _, body := range []string{`null`, `{}`, `{"version":1,"version":1}`, `{"Version":1}`, `[]`} {
		os.WriteFile(path, []byte(body), 0600)
		if _, err := Load(path); err == nil {
			t.Fatal("invalid file accepted")
		}
	}
	c := Default()
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, append(raw, []byte(` {}`)...), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Version = 2 }, func(c *Config) { c.Models[0].MaxOutputTokens = c.Models[0].ContextLimit },
		func(c *Config) { c.Models[0].Backend.BaseURL = "http://example.com:8080" },
		func(c *Config) { c.Models[0].Backend.BaseURL = "http://127.0.0.1:8000" },
		func(c *Config) { c.Models[0].Backend.Engine = "unknown" },
		func(c *Config) { c.Models[0].Aliases = []string{"local-llm"} },
		func(c *Config) { c.Models[0].Launch = &Launch{} },
	} {
		next := Default()
		change(&next)
		if err := next.Validate(); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	for _, raw := range []string{"http://127.0.0.1", "http://user:pass@127.0.0.1:8080", "http://127.0.0.1:8080/v1", "http://127.0.0.1:8080?x=1", "https://127.0.0.1:8080"} {
		if _, err := LocalURL(raw); err == nil {
			t.Fatal("unsafe URL accepted", raw)
		}
	}
}

func TestCompatibleReload(t *testing.T) {
	c := Default()
	next := c.Clone()
	next.Models[0].Limits.Requests++
	if err := c.CompatibleReload(next); err != nil {
		t.Fatal(err)
	}
	next.Models[0].Backend.BaseURL = "http://127.0.0.1:8082"
	if err := c.CompatibleReload(next); err == nil {
		t.Fatal("runtime mutation accepted")
	}
}

func TestRuntimeTemplateAndIdentity(t *testing.T) {
	for _, scenario := range []string{"valid", "identity", "context", "bad tokens", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var generation atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/health":
					if scenario == "redirect" {
						http.Redirect(w, r, "/v1/chat/completions", 302)
						return
					}
					w.Write([]byte(`{"status":"ok"}`))
				case "/v1/models":
					name := "local-llm"
					if scenario == "identity" {
						name = "wrong"
					}
					json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": name}}})
				case "/slots":
					n := 4096
					if scenario == "context" {
						n = 512
					}
					json.NewEncoder(w).Encode([]map[string]int{{"n_ctx": n}})
				case "/apply-template":
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["messages"] == nil {
						t.Error("messages missing")
					}
					w.Write([]byte(`{"prompt":"<chat>hello</chat>"}`))
				case "/tokenize":
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["content"] != "<chat>hello</chat>" || body["add_special"] != true || body["parse_special"] != true {
						t.Error("template or specials missing")
					}
					if scenario == "bad tokens" {
						w.Write([]byte(`{"tokens":[null]}`))
					} else {
						w.Write([]byte(`{"tokens":[1,2,3,4,5]}`))
					}
				default:
					generation.Add(1)
					http.Error(w, "unexpected inference", 500)
				}
			}))
			defer backend.Close()
			m := Default().Models[0]
			m.Backend.BaseURL = backend.URL
			n, err := NewInspector().CountInput(context.Background(), m, []map[string]string{{"role": "user", "content": "hello"}})
			if scenario == "valid" {
				if err != nil || n != 5 {
					t.Fatal(n, err)
				}
			} else if err == nil {
				t.Fatal("invalid runtime accepted")
			}
			if generation.Load() != 0 {
				t.Fatal("generation or redirect reached")
			}
		})
	}
}

func TestInspectorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewInspector().CountInput(ctx, Default().Models[0], nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatal(err)
	}
}
