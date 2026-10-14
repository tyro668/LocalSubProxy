package fakemodel

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaChat(t *testing.T) {
	srv := httptest.NewServer((&Server{Models: []string{"m"}}).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi","images":["AAAA"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var text strings.Builder
	var last map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		last = nil
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		text.WriteString(last["message"].(map[string]any)["content"].(string))
	}
	if !strings.Contains(text.String(), "我看到了一张图片，3 字节。") || !strings.Contains(text.String(), "你说的是：hi") {
		t.Fatalf("reply %q", text.String())
	}
	if last["done"] != true || last["eval_count"].(float64) == 0 {
		t.Fatalf("final %v", last)
	}
	tags, _ := http.Get(srv.URL + "/api/tags")
	var tg struct{ Models []struct{ Name string } }
	_ = json.NewDecoder(tags.Body).Decode(&tg)
	if len(tg.Models) != 1 || tg.Models[0].Name != "m" {
		t.Fatalf("tags %+v", tg)
	}
}
