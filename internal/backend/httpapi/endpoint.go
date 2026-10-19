package httpapi

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/tyro668/avora/internal/backend"
)

func SafeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("URL must contain only scheme and host")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("HTTPS required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("plain HTTP is permitted only for a loopback IP")
	}
	return nil
}

// ValidateLocal checks a local HTTP backend (loopback or HTTPS URL).
func ValidateLocal(a *backend.Config, _ bool) error {
	if err := SafeURL(a.BaseURL); err != nil {
		return fmt.Errorf("adapter URL: %w", err)
	}
	return nil
}

// ValidateCloud checks a provider backend: its URL and an API key.
func ValidateCloud(a *backend.Config, _ bool) error {
	if err := SafeProviderURL(a.BaseURL); err != nil {
		return fmt.Errorf("adapter URL: %w", err)
	}
	if a.APIKey == "" || len(a.APIKey) > 4096 {
		return errors.New("API key required (maximum 4096 characters)")
	}
	return nil
}

func SafeProviderURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "..") {
		return errors.New("invalid provider URL")
	}
	u.Path = ""
	u.RawPath = ""
	return SafeURL(u.String())
}

func ProviderEndpoint(base, endpoint string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + endpoint
	}
	return base + "/v1" + endpoint
}

func Endpoint(a backend.Config, endpoint string) string {
	if a.Provider == "deepseek" {
		base := strings.TrimRight(a.BaseURL, "/")
		if base == "https://api.deepseek.com" || base == "https://api.deepseek.com/v1" {
			return "https://api.deepseek.com" + endpoint
		}
	}
	return ProviderEndpoint(a.BaseURL, endpoint)
}
