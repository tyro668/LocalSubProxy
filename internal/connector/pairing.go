package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type PairingCode struct {
	DeviceID  string    `json:"device_id"`
	ShortCode string    `json:"short_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func RequestPairing(ctx context.Context, serverURL, deviceID, deviceToken string) (PairingCode, error) {
	var code PairingCode
	if err := safeURL(serverURL); err != nil {
		return code, err
	}
	if deviceID == "" || len(deviceToken) < 32 {
		return code, errors.New("device ID and 32+ character token required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/v1/connector/pairing", nil)
	if err != nil {
		return code, err
	}
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return code, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return code, fmt.Errorf("pairing request failed: HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&code); err != nil {
		return code, err
	}
	if code.DeviceID != deviceID || len(code.ShortCode) != 8 {
		return PairingCode{}, errors.New("invalid pairing response")
	}
	return code, nil
}
