package conformance

// Z8: request parameters (third audit #2). Every case of the shared vector
// file spec/testvectors/params.json runs against the gateway: illegal values
// are rejected with 400 BAD_REQUEST and the offending parameter before
// anything reaches the device; legal values reach the device with exactly the
// expected task.run fields, and every task.run validates against
// spec/schemas/task.run.payload.json.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/spec/testvectors"
)

type paramVectors struct {
	Base  map[string]map[string]any `json:"base"`
	Cases []struct {
		Name      string         `json:"name"`
		Endpoint  string         `json:"endpoint"`
		Set       map[string]any `json:"set"`
		Remove    []string       `json:"remove"`
		Status    int            `json:"status"`
		Code      string         `json:"code"`
		Param     string         `json:"param"`
		Run       map[string]any `json:"run"`
		RunAbsent []string       `json:"run_absent"`
	} `json:"cases"`
}

func errCodeParam(b []byte) (string, string) {
	var e struct {
		Error struct {
			Code      string `json:"code"`
			Param     string `json:"param"`
			LMBPCode  string `json:"lmbp_code"`
			LMBPParam string `json:"lmbp_param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error.Code != "" {
		return e.Error.Code, e.Error.Param
	}
	return e.Error.LMBPCode, e.Error.LMBPParam
}

func jsonNorm(x any) any {
	b, _ := json.Marshal(x)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func (r *runner) auditParams(ctx context.Context, dev *extDev, model string) {
	var v paramVectors
	if err := json.Unmarshal(testvectors.Params, &v); err != nil || len(v.Cases) == 0 {
		r.check("Z8a", "spec/testvectors/params.json loads", fmt.Errorf("%v (%d cases)", err, len(v.Cases)))
		return
	}
	var badFail, okFail, schemaFail []string
	nBad, nOK, nRuns := 0, 0, 0
	for _, c := range v.Cases {
		// vectors of an undeclared profile do not apply (§13.1)
		setJSON, _ := json.Marshal(c.Set)
		if (c.Endpoint == "messages" && !r.declares(lmbp.ProfileAnthropic)) || (bytes.Contains(setJSON, []byte(`"tool`)) && !r.declares(lmbp.ProfileTools)) {
			continue
		}
		body := map[string]any{}
		for k, x := range v.Base[c.Endpoint] {
			body[k] = x
		}
		body["model"] = model
		for k, x := range c.Set {
			body[k] = x
		}
		for _, k := range c.Remove {
			delete(body, k)
		}
		name := c.Endpoint + "/" + c.Name
		cc, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		type res struct {
			st int
			b  []byte
		}
		ch := make(chan res, 1)
		go func() {
			var st int
			var b []byte
			if c.Endpoint == "messages" {
				st, _, b = r.messagesRaw(cc, body, nil)
			} else {
				st, _, b = r.chatRaw(cc, body, nil)
			}
			ch <- res{st, b}
		}()
		var run map[string]any
		var got res
	wait:
		for {
			select {
			case got = <-ch:
				break wait
			case f := <-dev.frames:
				if f.Type != lmbp.TypeTaskRun {
					continue
				}
				nRuns++
				if parts := strings.Split(f.JWS, "."); len(parts) == 3 {
					if p, err := lmbp.UnB64(parts[1]); err == nil {
						_ = json.Unmarshal(p, &run)
						if err := r.validate(r.payload, p); err != nil {
							schemaFail = append(schemaFail, fmt.Sprintf("%s: %v", name, err))
						}
					}
				}
				_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 1, lmbp.TaskDone{FinishReason: "stop"})
			case <-cc.Done():
				got = res{0, []byte("timeout")}
				break wait
			}
		}
		cancel()
		if c.Status != 200 {
			nBad++
			code, param := errCodeParam(got.b)
			switch {
			case got.st != c.Status || code != c.Code || param != c.Param:
				badFail = append(badFail, fmt.Sprintf("%s: %d %s param %q (want %d %s %q)", name, got.st, code, param, c.Status, c.Code, c.Param))
			case run != nil:
				badFail = append(badFail, name+": rejected but task.run reached the device")
			}
			continue
		}
		nOK++
		if got.st != 200 || run == nil {
			okFail = append(okFail, fmt.Sprintf("%s: status %d, task.run seen %v: %.120s", name, got.st, run != nil, got.b))
			continue
		}
		for k, want := range c.Run {
			if !reflect.DeepEqual(jsonNorm(run[k]), jsonNorm(want)) {
				okFail = append(okFail, fmt.Sprintf("%s: task.run %s = %v, want %v", name, k, run[k], want))
			}
		}
		for _, k := range c.RunAbsent {
			if _, has := run[k]; has {
				okFail = append(okFail, fmt.Sprintf("%s: task.run has %s", name, k))
			}
		}
	}
	list := func(xs []string) string {
		if len(xs) > 8 {
			return strings.Join(xs[:8], "; ") + fmt.Sprintf("; … (%d total)", len(xs))
		}
		return strings.Join(xs, "; ")
	}
	r.check("Z8a", fmt.Sprintf("%d illegal request parameters (params.json) are rejected with 400 BAD_REQUEST naming the parameter, before reaching the device", nBad),
		want(len(badFail) == 0, "%s", list(badFail)))
	r.check("Z8b", fmt.Sprintf("%d legal request parameters (params.json) reach the device with exactly the expected task.run fields", nOK),
		want(len(okFail) == 0, "%s", list(okFail)))
	r.check("Z8c", fmt.Sprintf("every task.run produced by the parameter vectors (%d) validates against task.run.payload.json", nRuns),
		want(len(schemaFail) == 0 && nRuns >= nOK, "%d runs: %s", nRuns, list(schemaFail)))
}
