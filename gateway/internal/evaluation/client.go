package evaluation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

type Result struct {
	RequestID    string   `json:"request_id"`
	JobID        string   `json:"job_id"`
	Class        string   `json:"class"`
	ScheduledMS  float64  `json:"scheduled_ms"`
	SentMS       *float64 `json:"sent_ms"`
	TerminalMS   float64  `json:"terminal_ms"`
	TTFTMS       *float64 `json:"ttft_ms"`
	E2EMS        *float64 `json:"e2e_ms"`
	OutputTokens *int     `json:"output_tokens"`
	UsageReason  string   `json:"usage_reason"`
	HTTPStatus   int      `json:"http_status"`
	Outcome      string   `json:"outcome"`
	Error        string   `json:"error,omitempty"`
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func newClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConnsPerHost = 128
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Perform times first text and [DONE] with Go's monotonic clock. A [DONE]
// followed by a broken HTTP body is still a failed request, never a success.
func Perform(ctx context.Context, client *http.Client, base, model, key, trialID, requestID string, job Job, trialStart time.Time, recorder *Recorder) (result Result) {
	started := time.Now()
	sent := milliseconds(started.Sub(trialStart))
	result = Result{RequestID: requestID, JobID: job.ID, Class: job.Class, ScheduledMS: job.AtSeconds * 1000, SentMS: &sent, Outcome: "failed", UsageReason: "usage_missing"}
	defer func() {
		result.TerminalMS = milliseconds(time.Since(trialStart))
		if ctx.Err() != nil {
			result.Outcome = "cancelled"
			if ctx.Err() == context.DeadlineExceeded {
				result.Outcome = "timed_out"
			}
		}
		if result.Outcome != "completed" {
			result.E2EMS = nil
		}
	}()
	payloadFields := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": job.Prompt}}, "max_tokens": job.MaxTokens, "temperature": 0, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	// request_class is a gateway JSON extension, not an HTTP header. Keep it
	// out of direct runtime requests, which only accept the inference payload.
	if key != "" {
		payloadFields["request_class"] = job.Class
	}
	payload, _ := json.Marshal(payloadFields)
	recorder.Emit("sent", map[string]any{"trial_id": trialID, "request_id": requestID, "sent_ms": sent, "body_sha256": digest(payload)})
	request, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		result.Error = err.Error()
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(request)
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 65536))
		result.Outcome = "rejected"
		result.Error = string(raw)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		result.Error = "expected text/event-stream"
		return
	}
	reader := io.LimitReader(response.Body, (16<<20)+1)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var dataLines []string
	finished, done, invalidUsage := false, false, false
	totalBytes, frames := 0, 0
	consume := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		frames++
		if frames > 8192 || len(data) > 256<<10 {
			return fmt.Errorf("SSE frame limit exceeded")
		}
		elapsed := milliseconds(time.Since(started))
		recorder.Emit("sse", map[string]any{"trial_id": trialID, "request_id": requestID, "at_ms": elapsed, "data": data})
		if done {
			return fmt.Errorf("data after [DONE]")
		}
		if data == "[DONE]" {
			if !finished {
				return fmt.Errorf("[DONE] without finish_reason")
			}
			done = true
			result.E2EMS = &elapsed
			return nil
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     *int `json:"prompt_tokens"`
				Completion *int `json:"completion_tokens"`
				Total      *int `json:"total_tokens"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return fmt.Errorf("invalid SSE JSON")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("stream error: %s", chunk.Error)
		}
		if len(chunk.Choices) > 1 {
			return fmt.Errorf("multiple choices are unsupported")
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return fmt.Errorf("unexpected choice index")
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				if finished {
					return fmt.Errorf("content after finish")
				}
				if result.TTFTMS == nil {
					result.TTFTMS = &elapsed
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
		if usage := chunk.Usage; usage != nil {
			if usage.Prompt == nil || usage.Completion == nil || usage.Total == nil || *usage.Prompt < 0 || *usage.Completion < 0 || *usage.Completion > job.MaxTokens || *usage.Total != *usage.Prompt+*usage.Completion {
				invalidUsage = true
				result.OutputTokens = nil
				result.UsageReason = "invalid_usage"
			} else if !invalidUsage {
				result.OutputTokens = usage.Completion
				result.UsageReason = "runtime_usage"
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		totalBytes += len(line) + 1
		if totalBytes > 16<<20 {
			result.Error = "stream size limit exceeded"
			return
		}
		if line == "" {
			if err := consume(); err != nil {
				result.Error = err.Error()
				return
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		result.Error = err.Error()
		return
	}
	if len(dataLines) > 0 || !done {
		result.Error = "incomplete SSE stream"
		return
	}
	result.Outcome = "completed"
	return
}
