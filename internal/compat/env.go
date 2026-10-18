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

// DiscoveryPath is the SaaS discovery document (lmbp-spec §6.1).
const DiscoveryPath = "/.well-known/avora"

// LegacyDiscoveryPath is the pre-rename discovery path. Devices fall back
// to it when a SaaS answers 404 on DiscoveryPath, and the bundled SaaS mocks
// still serve it, so SaaS deployments and devices can upgrade independently.
const LegacyDiscoveryPath = "/.well-known/localsubproxy"

// DiscoveryPaths lists the paths a device tries, in order.
var DiscoveryPaths = []string{DiscoveryPath, LegacyDiscoveryPath}
