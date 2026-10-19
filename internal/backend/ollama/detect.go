package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DetectContextWindow reads the model's trained context length from
// /api/show (model_info["<architecture>.context_length"]). It is the
// model's maximum; Ollama runs with a smaller num_ctx unless the scheme sets
// one, and silently truncates longer prompts, so a request over this length
// cannot be served correctly in any case.
func (x *Adapter) DetectContextWindow(ctx context.Context, model string) (int, string, error) {
	b, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(x.Config.BaseURL, "/")+"/api/show", bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := x.ProbeClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, "", fmt.Errorf("ollama /api/show returned %d", resp.StatusCode)
	}
	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&show); err != nil {
		return 0, "", err
	}
	arch, _ := show.ModelInfo["general.architecture"].(string)
	for k, v := range show.ModelInfo {
		if f, ok := v.(float64); ok && strings.HasSuffix(k, ".context_length") && (arch == "" || k == arch+".context_length") && f > 0 {
			return int(f), "Ollama /api/show " + k, nil
		}
	}
	return 0, "", fmt.Errorf("ollama /api/show has no context_length for %s", model)
}
