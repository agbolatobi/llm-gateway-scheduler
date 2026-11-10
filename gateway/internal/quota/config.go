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
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open quota config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > 1<<20 {
		return Config{}, fmt.Errorf("quota config exceeds 1 MiB")
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	var c Config
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode quota config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("quota config must contain one JSON object")
	}
	_, err = c.EffectiveGlobal()
	return c, err
}

func validLimits(v Limits) bool {
	return v.Requests > 0 && v.Tokens > 0 && v.Requests <= 1e15 && v.Tokens <= 1e15
}

func (c Config) requestWindow() time.Duration {
	return time.Duration(c.RequestWindowSeconds) * time.Second
}
func (c Config) tokenWindow() time.Duration { return time.Duration(c.TokenWindowSeconds) * time.Second }

func (c Config) EffectiveGlobal() (Limits, error) {
	if c.RequestWindowSeconds < 1 || c.RequestWindowSeconds > 86400 || c.TokenWindowSeconds < 1 || c.TokenWindowSeconds > 2592000 {
		return Limits{}, fmt.Errorf("request window must be 1..86400 seconds; token window 1..2592000 seconds")
	}
	if !validLimits(c.User) || !validLimits(c.GlobalCeiling) {
		return Limits{}, fmt.Errorf("user and ceiling limits must be positive and at most 10^15")
	}
	if len(c.Models) == 0 {
		return Limits{}, fmt.Errorf("at least one canonical model is required")
	}
	names := make(map[string]bool)
	var total Limits
	for _, model := range c.Models {
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
		total.Requests += min(model.Limits.Requests, c.GlobalCeiling.Requests-total.Requests)
		total.Tokens += min(model.Limits.Tokens, c.GlobalCeiling.Tokens-total.Tokens)
	}
	return total, nil
}
