package protocol

import (
	"strings"
	"testing"
)

func TestLargeInlinePayloadRequiresArtifact(t *testing.T) {
	req := CreateTask{Target: Target{DeviceID: "dev_1", AdapterID: "ollama", ModelID: "model"}, Operation: "chat.generate", Input: Input{Messages: []Message{{Role: "user", Content: strings.Repeat("x", 200<<10)}}}}
	if err := req.Validate(); err == nil {
		t.Fatal("payload too large for the WSS frame was accepted")
	}
	req.Input.Messages[0].Content = "summarize attachment"
	req.Input.ArtifactRefs = []string{"artifact_" + strings.Repeat("a", 32)}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
}
