// Package quota implements single-process, fixed-window inference allowances.
package quota

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type Limits struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

// Models are canonical quota registrations, not runnable backend definitions.
// Aliases contribute no extra allowance. GATE-04 will supply the runtime registry.
type Model struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	Limits  Limits   `json:"limits"`
}

type Config struct {
	RequestWindowSeconds int64   `json:"request_window_seconds"`
	TokenWindowSeconds   int64   `json:"token_window_seconds"`
	User                 Limits  `json:"user"`
	GlobalCeiling        Limits  `json:"global_ceiling"`
	Models               []Model `json:"models"`
}

func DefaultConfig() Config {
	return Config{
		RequestWindowSeconds: 60, TokenWindowSeconds: 3600,
		User:          Limits{Requests: 20, Tokens: 40000},
		GlobalCeiling: Limits{Requests: 60, Tokens: 200000},
		Models:        []Model{{ID: "local-llm", Limits: Limits{Requests: 30, Tokens: 100000}}},
	}
}

// Load rejects unknown fields, trailing JSON, oversized files and invalid limits.
func Load(path string) (Config, error) {
	configFile, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open quota config: %w", err)
	}
	defer configFile.Close()
	data, err := io.ReadAll(io.LimitReader(configFile, (1<<20)+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > 1<<20 {
		return Config{}, fmt.Errorf("quota config exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode quota config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("quota config must contain one JSON object")
	}
	_, err = config.EffectiveGlobal()
	return config, err
}

func validLimits(limits Limits) bool {
	return limits.Requests > 0 && limits.Tokens > 0 && limits.Requests <= 1e15 && limits.Tokens <= 1e15
}

func (config Config) requestWindow() time.Duration {
	return time.Duration(config.RequestWindowSeconds) * time.Second
}
func (config Config) tokenWindow() time.Duration {
	return time.Duration(config.TokenWindowSeconds) * time.Second
}

func (config Config) EffectiveGlobal() (Limits, error) {
	if config.RequestWindowSeconds < 1 || config.RequestWindowSeconds > 86400 || config.TokenWindowSeconds < 1 || config.TokenWindowSeconds > 2592000 {
		return Limits{}, fmt.Errorf("request window must be 1..86400 seconds; token window 1..2592000 seconds")
	}
	if !validLimits(config.User) || !validLimits(config.GlobalCeiling) {
		return Limits{}, fmt.Errorf("user and ceiling limits must be positive and at most 10^15")
	}
	if len(config.Models) == 0 {
		return Limits{}, fmt.Errorf("at least one canonical model is required")
	}
	names := make(map[string]bool)
	var total Limits
	for _, model := range config.Models {
		if !validLimits(model.Limits) {
			return Limits{}, fmt.Errorf("model limits must be positive and at most 10^15")
		}
		for _, name := range append([]string{model.ID}, model.Aliases...) {
			if strings.TrimSpace(name) != name || name == "" || names[name] {
				return Limits{}, fmt.Errorf("model IDs and aliases must be nonempty, unique, and trimmed")
			}
			names[name] = true
		}
		// Saturating sums avoid overflow and make the safety ceiling explicit.
		total.Requests += min(model.Limits.Requests, config.GlobalCeiling.Requests-total.Requests)
		total.Tokens += min(model.Limits.Tokens, config.GlobalCeiling.Tokens-total.Tokens)
	}
	return total, nil
}
