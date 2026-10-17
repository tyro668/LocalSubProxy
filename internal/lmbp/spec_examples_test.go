package lmbp

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// LMBP 2.0 P1-5: every example in the spec documents and contracts agrees
// with the strict JOSE rules (§5.2) and with the shared vectors. Run by
// spec/validate.sh. A compact JWS anywhere in an example source must pass the
// strict verifier of its typ with the vector keys; every frame example must
// match envelope.json (v 2) and equal the vector frame of the same name; pair
// links and frame snippets in prose use the 2.0 version.

var compactJWS = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+`)

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// exampleDocs are the prose documents whose snippets must use LMBP 2.0.
var exampleDocs = []string{
	"README.md", "docs/lmbp-spec.md", "docs/implementers-guide.md", "docs/headless.md", "docs/guide.md",
	"docs/implementations.md", "sdk/go/README.md", "sdk/node/README.md", "sdk/python/README.md", "sdk/java/README.md",
	"mock/node/README.md", "mock/python/README.md", "mock/java/README.md", "mock/rust/README.md", "mock/web/README.md",
	"spec/examples/node/lmbp.mjs", "spec/examples/python/lmbp.py", "spec/examples/rust/src/lib.rs",
}

// strictVerify checks one compact JWS against the §5.2 rules for its typ.
func strictVerify(jws string, keys map[string]ed25519.PublicKey, now time.Time) error {
	hb, err := UnB64(strings.SplitN(jws, ".", 2)[0])
	if err != nil {
		return err
	}
	var h struct {
		Typ string  `json:"typ"`
		Kid *string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return err
	}
	signer := map[string]string{TypAccessToken: "gateway", TypTask: "gateway", TypMetadata: "gateway",
		TypAssertion: "device", TypProof: "device", "lmbp-e2ee-key+jwt": "device"}[h.Typ]
	if signer == "" {
		return fmt.Errorf("unknown typ %q", h.Typ)
	}
	pub := keys[signer]
	kidRequired := h.Typ == TypAccessToken || h.Typ == TypTask || h.Typ == TypMetadata || h.Typ == "lmbp-e2ee-key+jwt"
	if kidRequired && h.Kid == nil {
		return fmt.Errorf("%s needs kid (§5.2)", h.Typ)
	}
	if h.Typ == TypProof && h.Kid != nil {
		return fmt.Errorf("proof JWT carries no kid (§5.2)")
	}
	p, err := VerifyJWS(pub, jws, h.Typ)
	if err != nil {
		return err
	}
	c := claimsOf(p)
	s := func(k string) string { v, _ := c.str(k); return v }
	switch h.Typ {
	case TypAccessToken:
		_, err = VerifyJWT(pub, jws, h.Typ, s("aud"), now)
	case TypAssertion:
		_, err = VerifyJWT(pub, jws, h.Typ, s("aud"), now)
	case TypProof:
		err = c.require([]string{"iat"}, []string{"aud", "jti", "ath"}, []string{"iss"})
	case TypTask:
		_, err = VerifyTaskRunAt(pub, s("task_id"), jws, now)
	case TypMetadata:
		_, err = VerifyMetadata(pub, jws, s("issuer"), s("tenant"), now)
	default:
		_, _, err = VerifyE2EEKey(pub, "", s("sub"), jws, now)
	}
	return err
}

// walkValid collects strings from a vector file, skipping negative entries.
func walkValid(v any, path string, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if strings.Contains(k, "negative") || strings.Contains(k, "invalid") {
				continue
			}
			walkValid(e, path+"."+k, out)
		}
	case []any:
		for i, e := range x {
			walkValid(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case string:
		for _, m := range compactJWS.FindAllString(x, -1) {
			out[m] = path
		}
	}
}

func TestSpecExamples(t *testing.T) {
	var vec struct {
		Now   int64             `json:"now"`
		Keys  map[string]vecKey `json:"keys"`
		Valid []struct {
			Name  string          `json:"name"`
			Frame json.RawMessage `json:"frame"`
		} `json:"valid_frames"`
	}
	vb := repoFile(t, "spec/testvectors/vectors.json")
	if err := json.Unmarshal(vb, &vec); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{}
	for n, k := range vec.Keys {
		p, _ := UnB64(k.Public)
		keys[n] = p
	}
	now := time.Unix(vec.Now, 0)

	// 1) every compact JWS in an example source passes the strict rules.
	found := map[string]string{}
	for _, f := range []string{"spec/testvectors/vectors.json", "spec/testvectors/jose.json"} {
		var doc any
		_ = json.Unmarshal(repoFile(t, f), &doc)
		walkValid(doc, f, found)
	}
	for _, f := range append([]string{"spec/asyncapi.yaml", "spec/openapi.yaml"}, exampleDocs...) {
		for _, m := range compactJWS.FindAllString(string(repoFile(t, f)), -1) {
			found[m] = f
		}
	}
	if len(found) < 10 {
		t.Fatalf("only %d JWS examples found; the scanner is broken", len(found))
	}
	t.Logf("%d compact JWS examples checked", len(found))
	for jws, where := range found {
		if err := strictVerify(jws, keys, now); err != nil {
			t.Errorf("%s: JWS example %.40s… fails the strict JOSE rules: %v", where, jws, err)
		}
	}

	// 2) contract frame examples: envelope.json (v 2) and equal to the vector frame.
	env := loadSchema(t, "envelope.json")
	byName := map[string]json.RawMessage{}
	for _, f := range vec.Valid {
		byName[f.Name] = f.Frame
	}
	name := regexp.MustCompile(`^\s*- name: "(.*)"$`)
	payload := regexp.MustCompile(`^\s*payload: (\{.*\})$`)
	examples := 0
	for _, f := range []string{"spec/asyncapi.yaml"} {
		cur := ""
		for _, line := range strings.Split(string(repoFile(t, f)), "\n") {
			if m := name.FindStringSubmatch(line); m != nil {
				cur = m[1]
			}
			m := payload.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			examples++
			if err := env.Validate(inst(t, []byte(m[1]))); err != nil {
				t.Errorf("%s example %q does not match envelope.json: %v", f, cur, err)
			}
			want, ok := byName[cur]
			var a, b any
			_ = json.Unmarshal([]byte(m[1]), &a)
			_ = json.Unmarshal(want, &b)
			if !ok || !reflect.DeepEqual(a, b) {
				t.Errorf("%s example %q differs from vectors.json valid_frames[%q]", f, cur, cur)
			}
		}
	}
	if examples < 5 {
		t.Fatalf("asyncapi has %d frame examples, want >= 5", examples)
	}

	// 3) prose: frame snippets and pair links use the 2.0 version; JSON frame
	// blocks match envelope.json.
	frameV := regexp.MustCompile(`\{\s*"?v"?\s*:\s*(\d+)\s*,\s*"?type"?`)
	link := regexp.MustCompile(`lmbp://pair\?v=(\d+)`)
	check := regexp.MustCompile(`frame(?:\.v|\["v"\]|\.get\("v"\))\s*!==?\s*(\d+)`)
	block := regexp.MustCompile("(?s)```json\n(.*?)```")
	// subprotocol selection snippets: subprotocol="x", protocols(["x"]), subprotocols=["x"], 选择 `x`
	sub := regexp.MustCompile("(?:subprotocols?\\s*=\\s*\\[?|protocols\\(\\[|选择 `)\"?(lmbp[.0-9]*)[\"`]")
	// The subprotocol stays "lmbp" in 2.0 (the version lives in the envelope v,
	// protocol_versions and the pair link): a versioned name such as lmbp.2 is wrong.
	versioned := regexp.MustCompile(`\blmbp\.[0-9]+\b`)
	for _, f := range append(append([]string{}, exampleDocs...), "spec/openapi.yaml", "spec/asyncapi.yaml", "docs/lmbp-2-p2-draft.md") {
		for _, m := range versioned.FindAll(repoFile(t, f), -1) {
			t.Errorf("%s: versioned subprotocol name %q; the subprotocol is %q (version is in envelope v)", f, m, Subprotocol)
		}
	}
	for _, f := range exampleDocs {
		b := repoFile(t, f)
		for _, m := range sub.FindAllSubmatch(b, -1) {
			if string(m[1]) != Subprotocol {
				t.Errorf("%s: subprotocol snippet %s selects %q, want %q", f, m[0], m[1], Subprotocol)
			}
		}
		for _, m := range frameV.FindAllSubmatch(b, -1) {
			if string(m[1]) != fmt.Sprint(Version) {
				t.Errorf("%s: frame snippet %s uses v %s, protocol is v %d", f, m[0], m[1], Version)
			}
		}
		for _, m := range check.FindAllSubmatch(b, -1) {
			if string(m[1]) != fmt.Sprint(Version) {
				t.Errorf("%s: version check %s compares with %s, protocol is v %d", f, m[0], m[1], Version)
			}
		}
		for _, m := range link.FindAllSubmatch(b, -1) {
			if string(m[1]) != PairLinkVersion {
				t.Errorf("%s: pair link %s uses v=%s, want v=%s", f, m[0], m[1], PairLinkVersion)
			}
		}
		for _, m := range block.FindAllSubmatch(b, -1) {
			var obj map[string]any
			if json.Unmarshal(bytes.TrimSpace(m[1]), &obj) != nil || obj["v"] == nil || obj["type"] == nil {
				continue
			}
			if err := env.Validate(inst(t, m[1])); err != nil {
				t.Errorf("%s: JSON frame example does not match envelope.json: %v", f, err)
			}
		}
	}
}

// The validator itself must reject a non-strict example (fails before the
// strict rules were applied to documentation examples).
func TestSpecExamplesRejectsLooseJWS(t *testing.T) {
	gpriv, _ := vkey("lmbp test vector: gateway key")
	gpub := gpriv.Public().(ed25519.PublicKey)
	keys := map[string]ed25519.PublicKey{"gateway": gpub}
	now := time.Unix(vecIat+10, 0)
	run := `{"task_id":"t","device_id":"d","model":"m","nonce":"0123456789abcdef","iat":` + fmt.Sprint(vecIat) + `,"exp":` + fmt.Sprint(vecIat+60) + `,"messages":[{"role":"user","content":"hi"}]}`
	sign := func(h string) string {
		in := B64([]byte(h)) + "." + B64([]byte(run))
		return in + "." + B64(ed25519.Sign(gpriv, []byte(in)))
	}
	kid := Thumbprint(gpub)
	if err := strictVerify(sign(`{"alg":"EdDSA","kid":"`+kid+`","typ":"lmbp-task+jws"}`), keys, now); err != nil {
		t.Fatalf("strict example rejected: %v", err)
	}
	for _, h := range []string{
		`{"alg":"EdDSA","typ":"lmbp-task+jws"}`,                                // kid missing
		`{"alg":"EdDSA","kid":"` + kid + `","typ":"lmbp-task+jws","b64":true}`, // extra member
		`{"alg":"EdDSA","kid":"wrong","typ":"lmbp-task+jws"}`,                  // kid not the thumbprint
	} {
		if strictVerify(sign(h), keys, now) == nil {
			t.Errorf("loose header accepted: %s", h)
		}
	}
}
