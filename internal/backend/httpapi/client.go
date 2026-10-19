package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyro668/avora/internal/backend"
)

type Client struct {
	Config      backend.Config
	ProbeClient *http.Client
	Slots       int
}

func (x *Client) Probe(ctx context.Context) backend.Health {
	a := x.Config
	h := backend.Health{AdapterID: a.ID, AdapterType: a.Type, Models: a.Models, Health: "unavailable", AuthState: "ready", InferenceLocation: "local", FreeSlots: x.Slots}
	cloud := a.Type == "openai_compatible" || a.Type == "anthropic_compatible"
	if cloud {
		h.InferenceLocation, h.AuthState = "provider_cloud", "configured"
	}
	if !a.Enabled {
		h.Health, h.FreeSlots = "disabled", 0
		return h
	}
	if cloud && a.SkipModelProbe {
		h.Health = "ready"
		return h
	}
	ids, err := x.Models(ctx)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	available := map[string]bool{}
	for _, m := range ids {
		available[m] = true
	}
	h.Models = nil
	for _, m := range a.Models {
		if available[m] {
			h.Models = append(h.Models, m)
		}
	}
	if len(h.Models) > 0 {
		h.Health = "ready"
	}
	return h
}

// listProviderModels returns the model IDs an HTTP adapter's provider
// reports: GET /models for cloud providers, /api/tags (name and model) for
// Ollama.
func (x *Client) Models(ctx context.Context) ([]string, error) {
	a := x.Config
	cloud := a.Type == "openai_compatible" || a.Type == "anthropic_compatible"
	probeURL := strings.TrimRight(a.BaseURL, "/") + "/api/tags"
	if cloud {
		probeURL = Endpoint(a, "/models")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", probeURL, nil)
	if err != nil {
		return nil, err
	}
	if a.Type == "openai_compatible" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	} else if a.Type == "anthropic_compatible" {
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	probeClient := *x.ProbeClient
	probeClient.Timeout = 5 * time.Second
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if cloud && (resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 501) {
			return nil, fmt.Errorf("model list endpoint unavailable (HTTP %d); if the service has no /models, enable \"skip model list probe\" for this scheme", resp.StatusCode)
		}
		return nil, fmt.Errorf("model list: HTTP %d", resp.StatusCode)
	}
	ids := []string{}
	if cloud {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
			return nil, fmt.Errorf("model list: %w", err)
		}
		for _, m := range v.Data {
			ids = append(ids, m.ID)
		}
	} else {
		var v struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
			return nil, fmt.Errorf("model list: %w", err)
		}
		for _, m := range v.Models {
			ids = append(ids, m.Name, m.Model)
		}
	}
	return ids, nil
}
