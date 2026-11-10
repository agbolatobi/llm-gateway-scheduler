package routing

import (
	"errors"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func TestSelectCopiesConfiguredDestination(t *testing.T) {
	m := registry.Default().Models[0]
	route, err := Select(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Backend.BaseURL = "http://127.0.0.1:8082"
	m.Backend.ExpectedModel = "other"
	if route.baseURL != "http://127.0.0.1:8080" || route.backendModel != "local-llm" || route.modelID != "local-llm" {
		t.Fatal("route did not preserve snapshot", route)
	}
}

func TestSelectRejectsIneligibleRoute(t *testing.T) {
	for _, mutate := range []func(*registry.Model){
		func(m *registry.Model) { m.Backend.BaseURL = "" },
		func(m *registry.Model) { m.Backend.BaseURL = "http://example.com:80" },
		func(m *registry.Model) { m.Backend.BaseURL = "http://secret@127.0.0.1:8080" },
		func(m *registry.Model) { m.Backend.BaseURL = "http://127.0.0.1:8080/custom" },
		func(m *registry.Model) { m.Backend.ExpectedModel = "" },
		func(m *registry.Model) { m.Backend.RuntimeID = "" },
		func(m *registry.Model) { m.Backend.Engine = "unknown" },
	} {
		m := registry.Default().Models[0]
		mutate(&m)
		_, err := Select(m)
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "route_unavailable" {
			t.Fatal(err)
		}
	}
}
