package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionProbeFailsClosed(testContext *testing.T) {
	for _, testCase := range []struct {
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
		testContext.Run(testCase.body, func(testContext *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Write([]byte(testCase.body))
			}))
			defer testServer.Close()
			model := Default().Models[0]
			model.Backend.BaseURL = testServer.URL
			prepared := NewExecutionProbe()
			defer prepared.Close()
			idle, err := prepared.Idle(context.Background(), model)
			if idle != testCase.idle || (err != nil) != testCase.invalid {
				testContext.Fatal(idle, err)
			}
		})
	}
}
func TestExecutionLimitsValidation(testContext *testing.T) {
	config := Default()
	if config.GlobalActiveLimit() != 1 {
		testContext.Fatal("unsafe default")
	}
	zero, two := 0, 2
	config.Gateway.MaxActiveRequests = &zero
	if config.Validate() == nil {
		testContext.Fatal("zero global accepted")
	}
	config.Gateway.MaxActiveRequests = &two
	if config.Validate() != nil {
		testContext.Fatal("valid global rejected")
	}
	config.Models[0].MaxActiveRequests = &two
	if config.Validate() == nil {
		testContext.Fatal("unsupported multi-slot accepted")
	}
	dir := testContext.TempDir()
	file := filepath.Join(dir, "config.json")
	os.WriteFile(file, []byte(`{"gateway":{"max_active_requests":null}}`), 0600)
	if _, err := Load(file); err == nil {
		testContext.Fatal("null capacity accepted")
	}
}
