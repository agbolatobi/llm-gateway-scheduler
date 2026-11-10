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
)

type Gateway struct {
	Listen            string `json:"listen"`
	MaxActiveRequests *int   `json:"max_active_requests,omitempty"`
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

func (c Config) GlobalActiveLimit() int {
	if c.Gateway.MaxActiveRequests == nil {
		return 1
	}
	return *c.Gateway.MaxActiveRequests
}

func Default() Config {
	q := quota.DefaultConfig()
	return Config{Version: 1, Gateway: Gateway{Listen: "127.0.0.1:8000"},
		Quotas: Quotas{q.RequestWindowSeconds, q.TokenWindowSeconds, q.User, q.GlobalCeiling},
		Models: []Model{{ID: "local-llm", Aliases: []string{"default"}, ContextLimit: 4096,
			DefaultOutputTokens: 128, MaxOutputTokens: 1024, Limits: q.Models[0].Limits,
			Backend: Backend{RuntimeID: "local-llm-runtime", Engine: "llama.cpp", BaseURL: "http://127.0.0.1:8080", ExpectedModel: "local-llm"}}}}
}

func (c Config) QuotaConfig() quota.Config {
	q := quota.Config{RequestWindowSeconds: c.Quotas.RequestWindowSeconds, TokenWindowSeconds: c.Quotas.TokenWindowSeconds, User: c.Quotas.User, GlobalCeiling: c.Quotas.GlobalCeiling}
	for _, m := range c.Models {
		q.Models = append(q.Models, quota.Model{ID: m.ID, Aliases: m.Aliases, Limits: m.Limits})
	}
	return q
}

// LocalURL accepts only explicit IPv4 loopback HTTP endpoints. No DNS discovery,
// proxy routing, credentials, arbitrary paths or redirect destinations are allowed.
func LocalURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return "", fmt.Errorf("backend must be a loopback HTTP URL with an explicit port and no path or credentials")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("backend port must be 1..65535")
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), nil
}

func (c Config) Validate() error {
	if c.GlobalActiveLimit() < 1 || c.GlobalActiveLimit() > 64 {
		return fmt.Errorf("global max_active_requests must be 1..64")
	}
	if c.Version != 1 {
		return fmt.Errorf("configuration version must be 1")
	}
	host, port, err := net.SplitHostPort(c.Gateway.Listen)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return fmt.Errorf("gateway listen must use IPv4 loopback and an explicit port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("invalid gateway listen port")
	}
	if _, err := c.QuotaConfig().EffectiveGlobal(); err != nil {
		return err
	}
	runtimes, addresses := map[string]bool{}, map[string]bool{}
	for _, m := range c.Models {
		if m.MaxActiveRequests != nil && *m.MaxActiveRequests != 1 {
			return fmt.Errorf("initial per-runtime max_active_requests must be 1")
		}
		if !safeID.MatchString(m.ID) || !safeID.MatchString(m.Backend.RuntimeID) {
			return fmt.Errorf("invalid model or runtime ID")
		}
		for _, a := range m.Aliases {
			if !safeID.MatchString(a) {
				return fmt.Errorf("invalid model alias")
			}
		}
		if m.ContextLimit < 2 || m.ContextLimit > 1<<24 || m.DefaultOutputTokens < 1 || m.MaxOutputTokens < m.DefaultOutputTokens || m.MaxOutputTokens >= m.ContextLimit {
			return fmt.Errorf("invalid context/output limits for %s", m.ID)
		}
		if m.Backend.Engine != "llama.cpp" || m.Backend.ExpectedModel == "" || strings.TrimSpace(m.Backend.ExpectedModel) != m.Backend.ExpectedModel {
			return fmt.Errorf("invalid backend engine or model identity for %s", m.ID)
		}
		address, err := LocalURL(m.Backend.BaseURL)
		if err != nil {
			return err
		}
		if address == "http://127.0.0.1:"+strconv.Itoa(p) {
			return fmt.Errorf("backend conflicts with gateway listen port")
		}
		if runtimes[m.Backend.RuntimeID] || addresses[address] {
			return fmt.Errorf("duplicate runtime or backend address; use aliases for the same model")
		}
		runtimes[m.Backend.RuntimeID], addresses[address] = true, true
		if l := m.Launch; l != nil {
			if strings.TrimSpace(l.Executable) == "" || strings.TrimSpace(l.ModelFile) == "" || strings.TrimSpace(l.Device) == "" || l.Parallel != 1 || l.Threads < 1 || l.Threads > 4096 {
				return fmt.Errorf("invalid launch settings for %s (initial managed runtimes require parallel=1)", m.ID)
			}
		}
	}
	return nil
}

// rejectDuplicates makes Go and Python reject ambiguous duplicate object keys.
func rejectDuplicates(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate configuration field")
				}
				seen[s] = true
				if s == "max_active_requests" {
					var raw json.RawMessage
					if err := d.Decode(&raw); err != nil {
						return err
					}
					if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
						return fmt.Errorf("max_active_requests cannot be null")
					}
					continue
				}
				if err := rejectDuplicates(d); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := rejectDuplicates(d); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("cannot open gateway config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, fmt.Errorf("cannot read gateway config or file exceeds 1 MiB")
	}
	if err := rejectDuplicates(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return Config{}, fmt.Errorf("invalid or duplicate configuration JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var c Config
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("invalid configuration fields or types")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain one JSON object")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	for n := range c.Models {
		if l := c.Models[n].Launch; l != nil {
			if !filepath.IsAbs(l.Executable) {
				l.Executable = filepath.Join(root, l.Executable)
			}
			if !filepath.IsAbs(l.ModelFile) {
				l.ModelFile = filepath.Join(root, l.ModelFile)
			}
			if resolved, err := filepath.EvalSymlinks(l.Executable); err == nil {
				l.Executable = resolved
			}
			if resolved, err := filepath.EvalSymlinks(l.ModelFile); err == nil {
				l.ModelFile = resolved
			}
		}
	}
	return c, nil
}

func (c Config) Lookup(name string) (Model, bool) {
	for _, m := range c.Models {
		if name == m.ID {
			return m, true
		}
		for _, a := range m.Aliases {
			if name == a {
				return m, true
			}
		}
	}
	return Model{}, false
}
