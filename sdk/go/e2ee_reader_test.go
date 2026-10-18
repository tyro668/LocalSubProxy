package lmbpserver_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
	lmbpserver "github.com/tyro668/avora/sdk/go"
)

// B3: per-frame Open is stateless, so on its own it cannot see a gateway
// reorder, duplicate, drop or truncate frames. The response reader enforces
// seq order (last+1) and the sealed task.done terminator.

func sealedFrames(t *testing.T) (*lmbpserver.E2EESealed, []lmbpserver.E2EEFrame) {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	s := &lmbpserver.E2EESealed{RID: "rid_test", ResponseKey: key}
	var fs []lmbpserver.E2EEFrame
	for i, f := range []struct{ typ, pt string }{
		{lmbp.TypeTaskDelta, `{"text":"a"}`}, {lmbp.TypeTaskDelta, `{"text":"b"}`}, {lmbp.TypeTaskDone, `{"finish_reason":"stop"}`},
	} {
		seq := int64(i + 1)
		d := lmbp.SealChunk(key, s.RID, f.typ, seq, []byte(f.pt))
		fs = append(fs, lmbpserver.E2EEFrame{Seq: seq, Type: f.typ, Data: d})
	}
	return s, fs
}

func readAll(s *lmbpserver.E2EESealed, after int64, fs []lmbpserver.E2EEFrame) (string, error) {
	r := s.ReaderAfter(after)
	var out string
	for _, f := range fs {
		p, err := r.Next(f)
		if err != nil {
			return out, err
		}
		if f.Type == lmbp.TypeTaskDelta {
			d, _ := lmbpserver.DecodeE2EEDelta(p)
			out += d.Text
		}
	}
	return out, r.Finish()
}

func TestE2EEReaderEnforcesOrderAndTerminator(t *testing.T) {
	s, f := sealedFrames(t)
	if out, err := readAll(s, 0, f); err != nil || out != "ab" {
		t.Fatalf("in order: %q %v", out, err)
	}
	if out, err := readAll(s, 1, f[1:]); err != nil || out != "b" {
		t.Fatalf("resumed after seq 1: %q %v", out, err)
	}
	for name, c := range map[string][]lmbpserver.E2EEFrame{
		"reordered":           {f[1], f[0], f[2]},
		"duplicated":          {f[0], f[0], f[1], f[2]},
		"dropped middle":      {f[0], f[2]},
		"truncated (no done)": {f[0], f[1]},
		"after done":          {f[0], f[1], f[2], f[2]},
		"type swapped":        {f[0], f[1], {Seq: 3, Type: lmbp.TypeTaskDelta, Data: f[2].Data}},
	} {
		if _, err := readAll(s, 0, c); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "e2ee") {
			t.Errorf("%s: unhelpful error %v", name, err)
		}
	}
}

// Documents why the reader exists: Open alone accepts frames out of order
// (each frame authenticates only its own seq/type/rid).
func TestE2EEOpenIsStatelessPerFrame(t *testing.T) {
	s, f := sealedFrames(t)
	for _, i := range []int{1, 0, 0, 2} {
		if _, err := s.Open(f[i]); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
}
