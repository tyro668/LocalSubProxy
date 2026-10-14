package lmbp

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// spec/testvectors/jose.json: the shared strict-JOSE vector set (audit
// finding 4). Every implementation MUST accept each "valid" entry and MUST
// reject each "negative" entry, for every verify path it implements.
// Regenerate with: go test ./internal/lmbp -run TestJOSEVectors -update

type joseVec struct {
	Name    string         `json:"name"`
	Kind    string         `json:"kind"`
	Key     string         `json:"verify_with_key"`
	Context map[string]any `json:"context"`
	Compact string         `json:"compact"`
	Reason  string         `json:"must_reject_because,omitempty"`
}

type joseFile struct {
	Description string            `json:"description"`
	Now         int64             `json:"now"`
	Keys        map[string]vecKey `json:"keys"`
	Valid       []joseVec         `json:"valid"`
	Negative    []joseVec         `json:"negative"`
}

func josePath() string { return filepath.Join("..", "..", "spec", "testvectors", "jose.json") }

func cloneMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&out)
	return out
}

func genJOSE() joseFile {
	dk, dv := vkey("lmbp test vector: device key")
	gk, gv := vkey("lmbp test vector: gateway key")
	ok, _ := vkey("lmbp test vector: other key")
	devID := "dev_0192f5e0-7c1a-7cc0-9b4a-3f1d2e5a6b70"
	cred := "dc_0192f5e0-7c1a-7cc0-9b4a-000000000003"
	now := vecIat + 10
	xpub := bytes.Repeat([]byte{7}, 32)
	type base struct {
		kind, key, typ, kid string
		priv                ed25519.PrivateKey
		claims              map[string]any
		ctx                 map[string]any
		ints, strs          []string // required NumericDate / non-empty string claims
	}
	md := map[string]any{}
	{
		m := GatewayMetadata{Issuer: vecIssuer, JWKSURI: vecIssuer + "/.well-known/jwks.json", DeviceAuthorizationEndpoint: vecIssuer + "/lmbp/v1/device/authorize",
			TokenEndpoint: vecIssuer + "/lmbp/v1/token", ConnectEndpoint: "wss://gw.example.com/lmbp/v1/connect", ProtocolVersions: []string{"1.0"}, Capabilities: Capabilities,
			ScopesSupported: Scopes, Tenant: "acme", Audience: TokenAudience(vecIssuer, "acme"), SaaS: &SaaSInfo{Name: "Acme CRM", PairingURI: "https://acme.example/lmbp/link"},
			Iat: vecIat, Exp: vecIat + 3600}
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &md)
	}
	bases := []base{
		{"proof", "device", TypProof, "", dk, map[string]any{"iss": devID, "aud": ProofHello, "iat": vecIat, "jti": "p_1", "ath": Ath(cred)},
			map[string]any{"aud": ProofHello, "iss": devID, "credential": cred}, []string{"iat"}, []string{"aud", "jti", "ath"}},
		{"client_assertion", "device", TypAssertion, dv.JKT, dk, map[string]any{"iss": devID, "sub": devID, "aud": AssertionAudience(vecIssuer), "iat": vecIat, "exp": vecIat + 60, "jti": "a_1"},
			map[string]any{"aud": AssertionAudience(vecIssuer)}, []string{"iat", "exp"}, []string{"iss", "sub", "aud", "jti"}},
		{"access_token", "gateway", TypAccessToken, gv.JKT, gk, map[string]any{"iss": vecIssuer, "sub": devID, "aud": TokenAudience(vecIssuer, "acme"), "tenant": "acme", "iat": vecIat, "exp": vecIat + 600, "jti": "jti_1", "cnf": map[string]any{"jkt": dv.JKT}},
			map[string]any{"aud": TokenAudience(vecIssuer, "acme")}, []string{"iat", "exp"}, []string{"iss", "sub", "aud", "jti"}},
		{"e2ee_key", "device", TypE2EEKey, dv.JKT, dk, map[string]any{"iss": devID, "sub": devID, "iat": vecIat, "exp": vecIat + 86400,
			"jwk": map[string]any{"kty": "OKP", "crv": "X25519", "x": B64(xpub), "kid": X25519Thumbprint(xpub), "use": "enc", "alg": "ECDH-ES"}},
			map[string]any{"device_id": devID}, []string{"iat", "exp"}, []string{"iss", "sub"}},
		{"task_run", "gateway", TypTask, gv.JKT, gk, map[string]any{"task_id": "task_1", "iat": vecIat, "exp": vecIat + 60, "device_id": devID, "model": "lm/qwen3-8b", "nonce": "n_9f86d081884c7d65",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "blobs": []any{map[string]any{"id": "b_1", "size": 3, "sha256": "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU", "media_type": "image/png"}}},
			map[string]any{"envelope_task_id": "task_1"}, []string{"iat", "exp"}, []string{"task_id", "device_id", "model", "nonce"}},
		{"metadata", "gateway", TypMetadata, gv.JKT, gk, md, map[string]any{"issuer": vecIssuer, "tenant": "acme"}, []string{"iat", "exp"}, []string{"issuer", "tenant", "audience"}},
	}
	sign := func(priv ed25519.PrivateKey, h map[string]any, c any) string {
		hb, _ := json.Marshal(h)
		cb, _ := json.Marshal(c)
		return rawJWS(priv, string(hb), cb)
	}
	f := joseFile{Description: "Strict JOSE vectors (spec §5.2). Protected header: exactly alg=EdDSA, typ=<expected>, optional kid (string, = RFC 7638 thumbprint of the verifying key); any other member (crit, b64, jku, ...) MUST be rejected. Claims: required NumericDates are JSON integers, required identifiers non-empty strings, exp >= iat. Verify at 'now' with the context given. Accept every valid entry; reject every negative entry.",
		Now: now, Keys: map[string]vecKey{}}
	_, f.Keys["device"] = vkey("lmbp test vector: device key")
	_, f.Keys["gateway"] = vkey("lmbp test vector: gateway key")
	for _, b := range bases {
		hdr := map[string]any{"alg": "EdDSA", "typ": b.typ}
		if b.kid != "" {
			hdr["kid"] = b.kid
		}
		vec := func(name, why string, h map[string]any, c any, priv ed25519.PrivateKey) joseVec {
			return joseVec{Name: b.kind + "/" + name, Kind: b.kind, Key: b.key, Context: b.ctx, Compact: sign(priv, h, c), Reason: why}
		}
		f.Valid = append(f.Valid, vec("valid", "", hdr, b.claims, b.priv))
		hmut := func(name, why string, mut func(h map[string]any)) {
			h := cloneMap(hdr)
			mut(h)
			f.Negative = append(f.Negative, vec(name, why, h, b.claims, b.priv))
		}
		hmut("b64_false", "b64:false (RFC 7797) is forbidden", func(h map[string]any) { h["b64"] = false })
		hmut("b64_false_crit", "b64:false with crit is forbidden", func(h map[string]any) { h["b64"] = false; h["crit"] = []string{"b64"} })
		hmut("crit", "crit is forbidden", func(h map[string]any) { h["crit"] = []string{"exp"}; h["exp"] = 1 })
		hmut("alg_es256", "alg must be EdDSA", func(h map[string]any) { h["alg"] = "ES256" })
		hmut("alg_none", "alg must be EdDSA", func(h map[string]any) { h["alg"] = "none" })
		hmut("alg_missing", "alg is required", func(h map[string]any) { delete(h, "alg") })
		hmut("typ_wrong", "explicit typing: typ must be "+b.typ, func(h map[string]any) { h["typ"] = "JOSE" })
		hmut("typ_missing", "typ is required", func(h map[string]any) { delete(h, "typ") })
		hmut("typ_wrong_type", "typ must be a string", func(h map[string]any) { h["typ"] = []string{b.typ} })
		hmut("unknown_header", "unknown protected header members are forbidden", func(h map[string]any) { h["x-extra"] = "1" })
		hmut("jku_header", "key-locating headers are forbidden", func(h map[string]any) { h["jku"] = "https://evil.example/jwks.json" })
		hmut("jwk_header", "embedded keys are forbidden", func(h map[string]any) { h["jwk"] = PublicJWK(ok.Public().(ed25519.PublicKey)) })
		hmut("kid_mismatch", "kid must be the verifying key's thumbprint", func(h map[string]any) { h["kid"] = Thumbprint(ok.Public().(ed25519.PublicKey)) })
		hmut("kid_wrong_type", "kid must be a string", func(h map[string]any) { h["kid"] = 7 })
		cmut := func(name, why string, mut func(c map[string]any)) {
			c := cloneMap(b.claims)
			mut(c)
			f.Negative = append(f.Negative, vec(name, why, hdr, c, b.priv))
		}
		for _, k := range b.ints {
			cmut(k+"_missing", k+" is required", func(c map[string]any) { delete(c, k) })
			cmut(k+"_string", k+" must be a JSON integer", func(c map[string]any) { c[k] = string(c[k].(json.Number)) })
			cmut(k+"_fraction", k+" must be a JSON integer", func(c map[string]any) { c[k] = json.Number(string(c[k].(json.Number)) + ".5") })
			cmut(k+"_bool", k+" must be a JSON integer", func(c map[string]any) { c[k] = true })
			cmut(k+"_negative", k+" must be >= 0", func(c map[string]any) { c[k] = -1 })
		}
		for _, k := range b.strs {
			cmut(k+"_missing", k+" is required", func(c map[string]any) { delete(c, k) })
			cmut(k+"_wrong_type", k+" must be a string", func(c map[string]any) { c[k] = 12345 })
			cmut(k+"_empty", k+" must be non-empty", func(c map[string]any) { c[k] = "" })
		}
		if len(b.ints) == 2 {
			cmut("exp_before_iat", "exp must be >= iat", func(c map[string]any) { c["exp"] = vecIat - 1 })
		}
		f.Negative = append(f.Negative, vec("payload_array", "payload must be a JSON object", hdr, []any{b.claims}, b.priv))
		f.Negative = append(f.Negative, vec("wrong_key", "signed by a key other than the verifying key", hdr, b.claims, ok))
		if b.kind == "task_run" {
			for _, m := range []struct {
				n, why string
				mut    func(x map[string]any)
			}{
				{"blob_size_string", "blob size must be an integer", func(x map[string]any) { x["size"] = "3" }},
				{"blob_size_negative", "blob size must be >= 0", func(x map[string]any) { x["size"] = -3 }},
				{"blob_id_missing", "blob id is required", func(x map[string]any) { delete(x, "id") }},
				{"blob_sha256_short", "blob sha256 must be 32 bytes base64url", func(x map[string]any) { x["sha256"] = "AAEC" }},
				{"blob_media_type_wrong_type", "blob media_type must be a string", func(x map[string]any) { x["media_type"] = 1 }},
			} {
				cmut(m.n, m.why, func(c map[string]any) { m.mut(c["blobs"].([]any)[0].(map[string]any)) })
			}
		}
	}
	return f
}

// verifyJOSEVector dispatches one vector to the Go verify path of its kind.
func verifyJOSEVector(v joseVec, keys map[string]ed25519.PublicKey, now time.Time) error {
	pub := keys[v.Key]
	s := func(k string) string { x, _ := v.Context[k].(string); return x }
	var err error
	switch v.Kind {
	case "proof":
		_, err = VerifyProof(pub, v.Compact, s("iss"), s("aud"), s("credential"), now)
	case "client_assertion":
		_, err = VerifyJWT(pub, v.Compact, TypAssertion, s("aud"), now)
	case "access_token":
		_, err = VerifyJWT(pub, v.Compact, TypAccessToken, s("aud"), now)
	case "e2ee_key":
		_, _, err = VerifyE2EEKey(pub, "", s("device_id"), v.Compact, now)
	case "task_run":
		_, err = VerifyTaskRunAt(pub, s("envelope_task_id"), v.Compact, now)
	case "metadata":
		_, err = VerifyMetadata(pub, v.Compact, s("issuer"), s("tenant"), now)
	}
	return err
}

func TestJOSEVectors(t *testing.T) {
	want, _ := json.MarshalIndent(genJOSE(), "", "  ")
	want = append(want, '\n')
	if *update {
		if err := os.WriteFile(josePath(), want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(josePath())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("spec/testvectors/jose.json is stale; run go test ./internal/lmbp -run TestJOSEVectors -update")
	}
	var f joseFile
	_ = json.Unmarshal(got, &f)
	keys := map[string]ed25519.PublicKey{}
	for n, k := range f.Keys {
		p, _ := UnB64(k.Public)
		keys[n] = p
	}
	now := time.Unix(f.Now, 0)
	for _, v := range f.Valid {
		if err := verifyJOSEVector(v, keys, now); err != nil {
			t.Errorf("valid %s rejected: %v", v.Name, err)
		}
	}
	for _, v := range f.Negative {
		if verifyJOSEVector(v, keys, now) == nil {
			t.Errorf("negative %s accepted (%s)", v.Name, v.Reason)
		}
	}
}
