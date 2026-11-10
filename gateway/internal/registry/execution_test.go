package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionProbeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		body          string
		idle, invalid bool
	}{
		{`[{"id":0,"is_processing":false}]`, true, false},
		{`[{"id":0,"is_processing":true}]`, false, false},
		{`[{"id":0}]`, false, true},
		{`[{"id":0,"is_processing":null}]`, false, true},
		{`[{"id":0,"is_processing":true,"is_processing":false}]`, false, true},
		{`[{"id":0,"is_processing":false},{"id":1,"is_processing":false}]`, false, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
			defer s.Close()
			m := Default().Models[0]
			m.Backend.BaseURL = s.URL
			p := NewExecutionProbe()
			defer p.Close()
			idle, err := p.Idle(context.Background(), m)
			if idle != tc.idle || (err != nil) != tc.invalid {
				t.Fatal(idle, err)
			}
		})
	}
}
func TestExecutionLimitsValidation(t *testing.T) {
	c := Default()
	if c.GlobalActiveLimit() != 1 {
		t.Fatal("unsafe default")
	}
	zero, two := 0, 2
	c.Gateway.MaxActiveRequests = &zero
	if c.Validate() == nil {
		t.Fatal("zero global accepted")
	}
	c.Gateway.MaxActiveRequests = &two
	if c.Validate() != nil {
		t.Fatal("valid global rejected")
	}
	c.Models[0].MaxActiveRequests = &two
	if c.Validate() == nil {
		t.Fatal("unsupported multi-slot accepted")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	os.WriteFile(file, []byte(`{"gateway":{"max_active_requests":null}}`), 0600)
	if _, err := Load(file); err == nil {
		t.Fatal("null capacity accepted")
	}
}
