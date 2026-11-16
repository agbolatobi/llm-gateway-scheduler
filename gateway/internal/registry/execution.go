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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &ExecutionProbe{client: &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (probe *ExecutionProbe) Close() { probe.client.CloseIdleConnections() }
func (probe *ExecutionProbe) Idle(ctx context.Context, model Model) (bool, error) {
	base, err := LocalURL(model.Backend.BaseURL)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", base+"/slots", nil)
	if err != nil {
		return false, err
	}
	response, err := probe.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("execution state unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false, fmt.Errorf("execution state unavailable")
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(responseBody) > 256<<10 {
		return false, fmt.Errorf("invalid execution state")
	}
	if rejectDuplicates(json.NewDecoder(bytes.NewReader(responseBody))) != nil {
		return false, fmt.Errorf("invalid execution state")
	}
	var slots []struct {
		ID         *int  `json:"id"`
		Processing *bool `json:"is_processing"`
	}
	if json.Unmarshal(responseBody, &slots) != nil || len(slots) != 1 || slots[0].ID == nil || *slots[0].ID != 0 || slots[0].Processing == nil {
		return false, fmt.Errorf("runtime must expose exactly one execution slot with processing state")
	}
	return !*slots[0].Processing, nil
}
