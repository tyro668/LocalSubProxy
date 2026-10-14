// Package testvectors embeds the shared cross-language test vectors.
package testvectors

import _ "embed"

// Params is params.json: legal and illegal request parameters (third audit #2).
//
//go:embed params.json
var Params []byte
