package gateway

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review9 P2-10: an Anthropic tool_result whose content array holds an image
// (e.g. a screenshot tool) lost everything but its text blocks: the image
// vanished silently and the model answered without it. Supported content is
// converted completely (images become image_url parts the device receives);
// unsupported content is rejected with a clear error.
func TestAnthropicToolResultImage(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG-fake-bytes"))
	conv := func(content any) map[string]any {
		return map[string]any{"model": "echo", "max_tokens": 64, "messages": []any{
			map[string]any{"role": "user", "content": "take a screenshot"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "screenshot", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": content}}},
		}}
	}
	code, body := postJSON(t, r.srv.URL+"/v1/messages", conv([]any{
		map[string]any{"type": "text", "text": "here it is"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
	}))
	if code != 200 {
		t.Fatalf("tool_result with image: %d %s", code, body)
	}
	if !strings.Contains(body, "image:image/png:15;") {
		t.Fatalf("the device never received the tool_result image: %s", body)
	}
	// image only: the tool message still says something, the image arrives
	code, body = postJSON(t, r.srv.URL+"/v1/messages", conv([]any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
	}))
	if code != 200 || !strings.Contains(body, "image:image/png:15;") {
		t.Fatalf("image-only tool_result: %d %s", code, body)
	}
	// unsupported nested content: a clear 400, never a silent drop
	code, body = postJSON(t, r.srv.URL+"/v1/messages", conv([]any{
		map[string]any{"type": "text", "text": "doc"},
		map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "x"}},
	}))
	if code != 400 || !strings.Contains(body, "tool_result") || !strings.Contains(body, "document") {
		t.Fatalf("unsupported tool_result content: %d %s", code, body)
	}
}
