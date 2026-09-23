package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type RemoteClaim struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	GatewayURL  string `json:"gateway_url"`
}

func pairingClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func CheckRemoteService(ctx context.Context, remoteURL string) error {
	if err := safeURL(remoteURL); err != nil {
		return fmt.Errorf("remote service URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(remoteURL, "/")+"/api/llm/v1/local/pairing-info", nil)
	if err != nil {
		return err
	}
	res, err := pairingClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 512)).Decode(&problem)
		if problem.Error != "" {
			return fmt.Errorf("remote service pairing unavailable: HTTP %d: %s", res.StatusCode, problem.Error)
		}
		return fmt.Errorf("remote service pairing unavailable: HTTP %d", res.StatusCode)
	}
	var info struct {
		DeviceID       string `json:"device_id"`
		PairingEnabled bool   `json:"pairing_enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&info); err != nil {
		return err
	}
	if !info.PairingEnabled || info.DeviceID == "" {
		return errors.New("invalid remote service pairing response")
	}
	return nil
}

func ClaimRemoteService(ctx context.Context, remoteURL, code string) (RemoteClaim, error) {
	var claim RemoteClaim
	if err := safeURL(remoteURL); err != nil {
		return claim, fmt.Errorf("remote service URL: %w", err)
	}
	if len(code) != 8 {
		return claim, errors.New("eight-digit verification code required")
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return claim, errors.New("eight-digit verification code required")
		}
	}
	body, _ := json.Marshal(map[string]string{"short_code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(remoteURL, "/")+"/api/llm/v1/local/claim", bytes.NewReader(body))
	if err != nil {
		return claim, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := pairingClient().Do(req)
	if err != nil {
		return claim, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return claim, fmt.Errorf("remote service claim failed: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&claim); err != nil {
		return claim, err
	}
	if claim.DeviceID == "" || len(claim.DeviceToken) < 32 {
		return RemoteClaim{}, errors.New("invalid remote service claim response")
	}
	if err := safeURL(claim.GatewayURL); err != nil {
		return RemoteClaim{}, fmt.Errorf("invalid gateway URL: %w", err)
	}
	return claim, nil
}
