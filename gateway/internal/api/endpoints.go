package api

import "llm-gateway-scheduler/gateway/internal/routing"

// Shared paths for gateway routes.
const (
	HealthPath          = "/health"
	ChatCompletionsPath = routing.ChatCompletionsPath
	ModelsPath          = "/v1/models"
)
