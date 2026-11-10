package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
)

// Inspector uses the running model's template and tokenizer, never a guessed
// words-to-tokens conversion. It does not send generation requests.
type Inspector struct{ client *http.Client }

func NewInspector() *Inspector {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Inspector{client: &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type RuntimeError struct{ Code string }

func (e *RuntimeError) Error() string { return e.Code }
func runtimeError(code string) error  { return &RuntimeError{Code: code} }

func (i *Inspector) query(ctx context.Context, base, path string, body any, result any) error {
	method := http.MethodGet
	var payload []byte
	if body != nil {
		method = http.MethodPost
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := i.client.Do(r)
	if err != nil {
		return runtimeError("model_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeError("model_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return runtimeError("model_metadata_invalid")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return runtimeError("model_metadata_invalid")
	}
	return nil
}

// CountInput bounds the entire validation exchange. Slot limits are per request,
// not the server's total allocated context. Template/tokenizer failures fail closed.
func (i *Inspector) CountInput(ctx context.Context, m Model, messages any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base, err := LocalURL(m.Backend.BaseURL)
	if err != nil {
		return 0, err
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := i.query(ctx, base, "/health", nil, &health); err != nil {
		return 0, err
	}
	if health.Status != "ok" {
		return 0, runtimeError("model_unavailable")
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := i.query(ctx, base, "/v1/models", nil, &models); err != nil {
		return 0, err
	}
	if len(models.Data) != 1 || models.Data[0].ID != m.Backend.ExpectedModel {
		return 0, runtimeError("model_identity_mismatch")
	}
	if m.Launch != nil {
		var props struct {
			ModelPath string `json:"model_path"`
		}
		if err := i.query(ctx, base, "/props", nil, &props); err != nil {
			return 0, err
		}
		if !filepath.IsAbs(props.ModelPath) || filepath.Clean(props.ModelPath) != filepath.Clean(m.Launch.ModelFile) {
			return 0, runtimeError("model_identity_mismatch")
		}
	}
	var slots []struct {
		Context int `json:"n_ctx"`
	}
	if err := i.query(ctx, base, "/slots", nil, &slots); err != nil {
		return 0, err
	}
	if len(slots) == 0 {
		return 0, runtimeError("model_context_mismatch")
	}
	for _, slot := range slots {
		if slot.Context < m.ContextLimit {
			return 0, runtimeError("model_context_mismatch")
		}
	}
	var formatted struct {
		Prompt *string `json:"prompt"`
	}
	if err := i.query(ctx, base, "/apply-template", map[string]any{"messages": messages}, &formatted); err != nil {
		return 0, err
	}
	if formatted.Prompt == nil {
		return 0, runtimeError("model_metadata_invalid")
	}
	var tokenized struct {
		Tokens []*int64 `json:"tokens"`
	}
	if err := i.query(ctx, base, "/tokenize", map[string]any{"content": *formatted.Prompt, "add_special": true, "parse_special": true}, &tokenized); err != nil {
		return 0, err
	}
	if len(tokenized.Tokens) == 0 {
		return 0, runtimeError("model_metadata_invalid")
	}
	for _, token := range tokenized.Tokens {
		if token == nil || *token < 0 {
			return 0, runtimeError("model_metadata_invalid")
		}
	}
	return len(tokenized.Tokens), nil
}

func (c Config) Clone() Config {
	copy := c
	if c.Gateway.MaxActiveRequests != nil {
		v := *c.Gateway.MaxActiveRequests
		copy.Gateway.MaxActiveRequests = &v
	}
	copy.Models = append([]Model(nil), c.Models...)
	for n, m := range c.Models {
		if m.MaxActiveRequests != nil {
			v := *m.MaxActiveRequests
			copy.Models[n].MaxActiveRequests = &v
		}
		copy.Models[n].Aliases = append([]string(nil), m.Aliases...)
		if m.Launch != nil {
			l := *m.Launch
			copy.Models[n].Launch = &l
		}
	}
	return copy
}

// CompatibleReload permits explicit registration/removal and policy changes,
// but never silently reinterprets an existing runtime's identity/context.
func (c Config) CompatibleReload(next Config) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if c.Gateway.Listen != next.Gateway.Listen {
		return fmt.Errorf("gateway listen changes require restart")
	}
	for _, old := range c.Models {
		for _, m := range next.Models {
			if old.ID == m.ID {
				if old.Backend != m.Backend || old.ContextLimit != m.ContextLimit {
					return fmt.Errorf("runtime or context changes require restart")
				}
				if (old.Launch == nil) != (m.Launch == nil) || (old.Launch != nil && *old.Launch != *m.Launch) {
					return fmt.Errorf("launch setting changes require restart")
				}
			}
		}
	}
	return nil
}
