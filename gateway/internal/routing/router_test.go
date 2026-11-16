package routing

import (
	"errors"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func TestSelectCopiesConfiguredDestination(testContext *testing.T) {
	model := registry.Default().Models[0]
	route, err := Select(model)
	if err != nil {
		testContext.Fatal(err)
	}
	model.Backend.BaseURL = "http://127.0.0.1:8082"
	model.Backend.ExpectedModel = "other"
	if route.baseURL != "http://127.0.0.1:8080" || route.backendModel != "local-llm" || route.modelID != "local-llm" {
		testContext.Fatal("route did not preserve snapshot", route)
	}
}

func TestSelectRejectsIneligibleRoute(testContext *testing.T) {
	for _, mutate := range []func(*registry.Model){
		func(model *registry.Model) { model.Backend.BaseURL = "" },
		func(model *registry.Model) { model.Backend.BaseURL = "http://example.com:80" },
		func(model *registry.Model) { model.Backend.BaseURL = "http://secret@127.0.0.1:8080" },
		func(model *registry.Model) { model.Backend.BaseURL = "http://127.0.0.1:8080/custom" },
		func(model *registry.Model) { model.Backend.ExpectedModel = "" },
		func(model *registry.Model) { model.Backend.RuntimeID = "" },
		func(model *registry.Model) { model.Backend.Engine = "unknown" },
	} {
		model := registry.Default().Models[0]
		mutate(&model)
		_, err := Select(model)
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "route_unavailable" {
			testContext.Fatal(err)
		}
	}
}
