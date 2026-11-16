// Package registry defines the shared gateway/launcher configuration.
package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/scheduler"
)

type Gateway struct {
	Scheduler         *scheduler.Config `json:"scheduler,omitempty"`
	Listen            string            `json:"listen"`
	MaxActiveRequests *int              `json:"max_active_requests,omitempty"`
}
type Quotas struct {
	RequestWindowSeconds int64        `json:"request_window_seconds"`
	TokenWindowSeconds   int64        `json:"token_window_seconds"`
	User                 quota.Limits `json:"user"`
	GlobalCeiling        quota.Limits `json:"global_ceiling"`
}
type Backend struct {
	RuntimeID     string `json:"runtime_id"`
	Engine        string `json:"engine"`
	BaseURL       string `json:"base_url"`
	ExpectedModel string `json:"expected_model"`
}
type Launch struct {
	Executable string `json:"executable"`
	ModelFile  string `json:"model_file"`
	Device     string `json:"device"`
	Parallel   int    `json:"parallel"`
	Threads    int    `json:"threads"`
}
type Model struct {
	ID                  string       `json:"id"`
	Aliases             []string     `json:"aliases,omitempty"`
	ContextLimit        int          `json:"context_limit"`
	DefaultOutputTokens int          `json:"default_output_tokens"`
	MaxOutputTokens     int          `json:"max_output_tokens"`
	Limits              quota.Limits `json:"limits"`
	Backend             Backend      `json:"backend"`
	Launch              *Launch      `json:"launch,omitempty"`
	MaxActiveRequests   *int         `json:"max_active_requests,omitempty"`
}
type Config struct {
	Version int     `json:"version"`
	Gateway Gateway `json:"gateway"`
	Quotas  Quotas  `json:"quotas"`
	Models  []Model `json:"models"`
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (config Config) SchedulerConfig() scheduler.Config {
	if config.Gateway.Scheduler == nil {
		return scheduler.Default()
	}
	return *config.Gateway.Scheduler
}

func (config Config) GlobalActiveLimit() int {
	if config.Gateway.MaxActiveRequests == nil {
		return 1
	}
	return *config.Gateway.MaxActiveRequests
}

func Default() Config {
	quotaConfig := quota.DefaultConfig()
	return Config{Version: 1, Gateway: Gateway{Listen: "127.0.0.1:8000"},
		Quotas: Quotas{quotaConfig.RequestWindowSeconds, quotaConfig.TokenWindowSeconds, quotaConfig.User, quotaConfig.GlobalCeiling},
		Models: []Model{{ID: "local-llm", Aliases: []string{"default"}, ContextLimit: 4096,
			DefaultOutputTokens: 128, MaxOutputTokens: 1024, Limits: quotaConfig.Models[0].Limits,
			Backend: Backend{RuntimeID: "local-llm-runtime", Engine: "llama.cpp", BaseURL: "http://127.0.0.1:8080", ExpectedModel: "local-llm"}}}}
}

func (config Config) QuotaConfig() quota.Config {
	quotaConfig := quota.Config{RequestWindowSeconds: config.Quotas.RequestWindowSeconds, TokenWindowSeconds: config.Quotas.TokenWindowSeconds, User: config.Quotas.User, GlobalCeiling: config.Quotas.GlobalCeiling}
	for _, model := range config.Models {
		quotaConfig.Models = append(quotaConfig.Models, quota.Model{ID: model.ID, Aliases: model.Aliases, Limits: model.Limits})
	}
	return quotaConfig
}

// LocalURL accepts only explicit IPv4 loopback HTTP endpoints. No DNS discovery,
// proxy routing, credentials, arbitrary paths or redirect destinations are allowed.
func LocalURL(raw string) (string, error) {
	parsedURL, err := url.Parse(raw)
	if err != nil || parsedURL.Scheme != "http" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.ForceQuery || parsedURL.Fragment != "" || parsedURL.RawPath != "" || (parsedURL.Path != "" && parsedURL.Path != "/") || (parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "localhost") {
		return "", fmt.Errorf("backend must be a loopback HTTP URL with an explicit port and no path or credentials")
	}
	port, err := strconv.Atoi(parsedURL.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("backend port must be 1..65535")
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), nil
}

func (config Config) Validate() error {
	if err := config.SchedulerConfig().Validate(); err != nil {
		return err
	}
	if config.GlobalActiveLimit() < 1 || config.GlobalActiveLimit() > 64 {
		return fmt.Errorf("global max_active_requests must be 1..64")
	}
	if config.Version != 1 {
		return fmt.Errorf("configuration version must be 1")
	}
	host, port, err := net.SplitHostPort(config.Gateway.Listen)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return fmt.Errorf("gateway listen must use IPv4 loopback and an explicit port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("invalid gateway listen port")
	}
	if _, err := config.QuotaConfig().EffectiveGlobal(); err != nil {
		return err
	}
	runtimes, addresses := map[string]bool{}, map[string]bool{}
	for _, model := range config.Models {
		if model.MaxActiveRequests != nil && *model.MaxActiveRequests != 1 {
			return fmt.Errorf("initial per-runtime max_active_requests must be 1")
		}
		if !safeID.MatchString(model.ID) || !safeID.MatchString(model.Backend.RuntimeID) {
			return fmt.Errorf("invalid model or runtime ID")
		}
		for _, alias := range model.Aliases {
			if !safeID.MatchString(alias) {
				return fmt.Errorf("invalid model alias")
			}
		}
		if model.ContextLimit < 2 || model.ContextLimit > 1<<24 || model.DefaultOutputTokens < 1 || model.MaxOutputTokens < model.DefaultOutputTokens || model.MaxOutputTokens >= model.ContextLimit {
			return fmt.Errorf("invalid context/output limits for %s", model.ID)
		}
		if model.Backend.Engine != "llama.cpp" || model.Backend.ExpectedModel == "" || strings.TrimSpace(model.Backend.ExpectedModel) != model.Backend.ExpectedModel {
			return fmt.Errorf("invalid backend engine or model identity for %s", model.ID)
		}
		address, err := LocalURL(model.Backend.BaseURL)
		if err != nil {
			return err
		}
		if address == "http://127.0.0.1:"+strconv.Itoa(portNumber) {
			return fmt.Errorf("backend conflicts with gateway listen port")
		}
		if runtimes[model.Backend.RuntimeID] || addresses[address] {
			return fmt.Errorf("duplicate runtime or backend address; use aliases for the same model")
		}
		runtimes[model.Backend.RuntimeID], addresses[address] = true, true
		if launchConfig := model.Launch; launchConfig != nil {
			if strings.TrimSpace(launchConfig.Executable) == "" || strings.TrimSpace(launchConfig.ModelFile) == "" || strings.TrimSpace(launchConfig.Device) == "" || launchConfig.Parallel != 1 || launchConfig.Threads < 1 || launchConfig.Threads > 4096 {
				return fmt.Errorf("invalid launch settings for %s (initial managed runtimes require parallel=1)", model.ID)
			}
		}
	}
	return nil
}

// rejectDuplicates makes Go and Python reject ambiguous duplicate object keys.
func rejectDuplicates(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate configuration field")
				}
				seen[key] = true
				if key == "scheduler" {
					var raw json.RawMessage
					if err := decoder.Decode(&raw); err != nil {
						return err
					}
					if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
						return fmt.Errorf("scheduler cannot be null")
					}
					if err := rejectDuplicates(json.NewDecoder(bytes.NewReader(raw))); err != nil {
						return err
					}
					continue
				}
				if key == "max_active_requests" {
					var raw json.RawMessage
					if err := decoder.Decode(&raw); err != nil {
						return err
					}
					if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
						return fmt.Errorf("max_active_requests cannot be null")
					}
					continue
				}
				if err := rejectDuplicates(decoder); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := rejectDuplicates(decoder); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	return nil
}

func Load(path string) (Config, error) {
	configFile, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("cannot open gateway config: %w", err)
	}
	defer configFile.Close()
	data, err := io.ReadAll(io.LimitReader(configFile, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, fmt.Errorf("cannot read gateway config or file exceeds 1 MiB")
	}
	if err := rejectDuplicates(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return Config{}, fmt.Errorf("invalid or duplicate configuration JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("invalid configuration fields or types")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain one JSON object")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	for modelIndex := range config.Models {
		if launchConfig := config.Models[modelIndex].Launch; launchConfig != nil {
			if !filepath.IsAbs(launchConfig.Executable) {
				launchConfig.Executable = filepath.Join(root, launchConfig.Executable)
			}
			if !filepath.IsAbs(launchConfig.ModelFile) {
				launchConfig.ModelFile = filepath.Join(root, launchConfig.ModelFile)
			}
			if resolved, err := filepath.EvalSymlinks(launchConfig.Executable); err == nil {
				launchConfig.Executable = resolved
			}
			if resolved, err := filepath.EvalSymlinks(launchConfig.ModelFile); err == nil {
				launchConfig.ModelFile = resolved
			}
		}
	}
	return config, nil
}

func (config Config) Lookup(name string) (Model, bool) {
	for _, model := range config.Models {
		if name == model.ID {
			return model, true
		}
		for _, alias := range model.Aliases {
			if name == alias {
				return model, true
			}
		}
	}
	return Model{}, false
}
