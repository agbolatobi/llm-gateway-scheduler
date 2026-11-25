package evaluation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func query(ctx context.Context, client *http.Client, base, path string, body any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	method := "GET"
	var payload []byte
	if body != nil {
		method = "POST"
		payload, _ = json.Marshal(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("%s returned HTTP %d", path, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return fmt.Errorf("metadata exceeds size limit")
	}
	return json.Unmarshal(raw, result)
}

type runtimeSlot struct {
	ID         int   `json:"id"`
	Processing *bool `json:"is_processing"`
	Context    int   `json:"n_ctx"`
}

func idle(ctx context.Context, client *http.Client, model registry.Model) error {
	var slots []runtimeSlot
	if err := query(ctx, client, model.Backend.BaseURL, "/slots", nil, &slots); err != nil {
		return err
	}
	if len(slots) != 1 || slots[0].ID != 0 || slots[0].Processing == nil || *slots[0].Processing || slots[0].Context < model.ContextLimit {
		return fmt.Errorf("evaluation requires one idle slot with the configured context limit; stop other generation traffic")
	}
	return nil
}
func resetCache(ctx context.Context, client *http.Client, model registry.Model) (any, error) {
	var result struct {
		ID     *int `json:"id_slot"`
		Erased *int `json:"n_erased"`
	}
	err := query(ctx, client, model.Backend.BaseURL, "/slots/0?action=erase", map[string]any{}, &result)
	if err == nil && (result.ID == nil || *result.ID != 0 || result.Erased == nil || *result.Erased < 0) {
		err = fmt.Errorf("invalid slot erase response")
	}
	return result, err
}
func snapshot(ctx context.Context, client *http.Client, model registry.Model, hashModel bool) (map[string]any, error) {
	result := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(), "go_version": runtime.Version()}
	for _, endpoint := range []string{"/health", "/props", "/slots", "/v1/models"} {
		var value any
		if err := query(ctx, client, model.Backend.BaseURL, endpoint, nil, &value); err != nil {
			return nil, err
		}
		result[endpoint] = value
	}
	for _, path := range []string{"/proc/cpuinfo", "/proc/meminfo", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/memory.max"} {
		if contents, err := os.ReadFile(path); err == nil {
			result[path] = string(contents)
		}
	}
	if hashModel {
		result["model_sha256"] = nil
		result["model_hash_reason"] = "model file not readable"
		if props, ok := result["/props"].(map[string]any); ok {
			if path, ok := props["model_path"].(string); ok {
				if modelFile, err := os.Open(path); err == nil {
					hasher := sha256.New()
					_, copyErr := io.Copy(hasher, modelFile)
					modelFile.Close()
					if copyErr == nil {
						result["model_sha256"] = hex.EncodeToString(hasher.Sum(nil))
						result["model_hash_reason"] = "file_sha256"
					}
				}
			}
		}
	}
	return result, nil
}
