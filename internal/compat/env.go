// Package compat holds the shims for the product rename (LocalSubProxy →
// Avora, 2.1.x). They exist for one release so existing setups keep
// working; remove the package afterwards.
package compat

import (
	"log"
	"os"
	"strings"
	"sync"
)

// LegacyEnvPrefix is the pre-rename prefix of the app's environment variables.
const LegacyEnvPrefix = "LOCALSUBPROXY_"

var warned sync.Map

// Getenv reads an AVORA_* variable. When it is unset, the pre-rename
// LOCALSUBPROXY_* variable of the same suffix is used instead, with a
// deprecation warning (once per variable).
func Getenv(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	suffix, ok := strings.CutPrefix(key, "AVORA_")
	if !ok {
		return ""
	}
	old := LegacyEnvPrefix + suffix
	v, ok := os.LookupEnv(old)
	if !ok {
		return ""
	}
	if _, done := warned.LoadOrStore(old, true); !done {
		log.Printf("warning: %s is deprecated and will stop working in the next release; set %s instead", old, key)
	}
	return v
}

// LegacyLMBPEnvPrefix is the pre-2.1 prefix of the reference gateway's
// environment variables (signing seed, data key, tenant API keys).
const LegacyLMBPEnvPrefix = "LSP_"

// LookupLMBP reads an LMBP_* variable of the reference gateway. When it is
// unset, the pre-2.1 LSP_* variable of the same suffix is used instead, with
// a deprecation warning (once per variable). Names without the LMBP_ prefix
// are read as is.
func LookupLMBP(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}
	suffix, ok := strings.CutPrefix(key, "LMBP_")
	if !ok {
		return "", false
	}
	old := LegacyLMBPEnvPrefix + suffix
	v, ok := os.LookupEnv(old)
	if !ok {
		return "", false
	}
	if _, done := warned.LoadOrStore(old, true); !done {
		log.Printf("warning: %s is deprecated and will stop working in the next release; set %s instead", old, key)
	}
	return v, true
}

// GetenvLMBP is LookupLMBP without the presence flag.
func GetenvLMBP(key string) string { v, _ := LookupLMBP(key); return v }

// DiscoveryPath is the SaaS discovery document (lmbp-spec §6.1).
const DiscoveryPath = "/.well-known/avora"

// LegacyDiscoveryPath is the pre-rename discovery path. Devices fall back
// to it when a SaaS answers 404 on DiscoveryPath, and the bundled SaaS mocks
// still serve it, so SaaS deployments and devices can upgrade independently.
const LegacyDiscoveryPath = "/.well-known/localsubproxy"

// DiscoveryPaths lists the paths a device tries, in order.
var DiscoveryPaths = []string{DiscoveryPath, LegacyDiscoveryPath}
