package demo

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
}
