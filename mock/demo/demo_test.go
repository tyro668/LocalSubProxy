package demo

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

func TestEndToEnd(t *testing.T) {
	// The demo pushes ~16 MB (60k chunks) through every layer with per-frame
	// SQLite persistence; -race slows that I/O path ~5-10x. Generous budget:
	// the real slowness (O(n²) stream cursor) is fixed in the gateway.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := Run(ctx, io.Discard, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.StreamText, "帮我写一句问候") {
		t.Fatalf("stream text %q", r.StreamText)
	}
	if r.CancelState != "CANCELLED" || r.UpstreamErrCode != "UPSTREAM_ERROR" || r.NotFoundCode != "MODEL_NOT_FOUND" {
		t.Fatalf("unexpected %+v", r)
	}
	if r.BobModels != 0 || r.BobRevokeStatus != 404 || r.PromptInDB || !r.RevokedRefresh || r.TokenRefreshes < 1 {
		t.Fatalf("isolation/encryption/revoke %+v", r)
	}
	if !r.ResumeLossless || r.Resumes < 2 {
		t.Fatalf("resume lossless=%v resumes=%d", r.ResumeLossless, r.Resumes)
	}
	if r.BackpressureAt <= 0 || r.BackpressureAt >= fakemodel.VeryLongChunks || r.BackpressureAll != fakemodel.VeryLongChunks {
		t.Fatalf("backpressure stalled at %d, delivered %d", r.BackpressureAt, r.BackpressureAll)
	}
	if !strings.Contains(r.ToolCall, "get_weather") || !strings.Contains(r.ToolAnswer, "晴，22°C") {
		t.Fatalf("tools %q %q", r.ToolCall, r.ToolAnswer)
	}
	if !strings.Contains(r.ImageReply, "image/png") {
		t.Fatalf("image %q", r.ImageReply)
	}
	if !strings.Contains(r.E2EEText, "青鸾") || r.E2EEPlainInStore || r.E2EEPlainInLogs || !r.E2EECiphertext {
		t.Fatalf("e2ee %+v", r)
	}
}
