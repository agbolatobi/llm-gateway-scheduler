package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ExecutionProbe supports the initial, exclusive single-slot llama.cpp runtime.
// Callers must send all generation through one gateway process for this contract.
type ExecutionProbe struct{ client *http.Client }

func NewExecutionProbe() *ExecutionProbe {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &ExecutionProbe{client: &http.Client{Transport: t, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *ExecutionProbe) Close() { p.client.CloseIdleConnections() }
func (p *ExecutionProbe) Idle(ctx context.Context, m Model) (bool, error) {
	base, err := LocalURL(m.Backend.BaseURL)
	if err != nil {
		return false, err
	}
	r, err := http.NewRequestWithContext(ctx, "GET", base+"/slots", nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(r)
	if err != nil {
		return false, fmt.Errorf("execution state unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("execution state unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(b) > 256<<10 {
		return false, fmt.Errorf("invalid execution state")
	}
	if rejectDuplicates(json.NewDecoder(bytes.NewReader(b))) != nil {
		return false, fmt.Errorf("invalid execution state")
	}
	var slots []struct {
		ID         *int  `json:"id"`
		Processing *bool `json:"is_processing"`
	}
	if json.Unmarshal(b, &slots) != nil || len(slots) != 1 || slots[0].ID == nil || *slots[0].ID != 0 || slots[0].Processing == nil {
		return false, fmt.Errorf("runtime must expose exactly one execution slot with processing state")
	}
	return !*slots[0].Processing, nil
}
