package main

import (
	"strings"
	"testing"
	"time"
)

func TestModelSSEDecoderFraming(t *testing.T) {
	valid := `{"type":"response.created","response":{"id":"r1","model": "model-a"}}`
	for _, tc := range []struct{ name, body, want string }{
		{"lf", "data: " + valid + "\n\n", "model-a"},
		{"crlf", "event: response.created\r\ndata: " + valid + "\r\n\r\n", "model-a"},
		{"cr", "data: " + valid + "\r\r", "model-a"},
		{"multiline", "data: {\n" + "data: \"type\":\"response.created\",\n" + "data: \"response\":{\"id\":\"r1\",\"model\":\"model-a\"}}\n\n", "model-a"},
		{"unrelated_model", "data: {\"type\":\"response.output_item.added\",\"item\":{\"model\":\"wrong\"}}\n\ndata: " + valid + "\n\n", "model-a"},
		{"missing_id", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"model-a\"}}\n\n", ""},
		{"wrong_type", "data: {\"type\":\"other\",\"response\":{\"id\":\"r1\",\"model\":\"wrong\"}}\n\n", ""},
		{"event_mismatch", "event: other\ndata: " + valid + "\n\n", ""},
		{"unfinished_event", "data: " + valid, ""},
		{"invalid_json", "data: {broken}\n\n", ""},
		{"done", "data: [DONE]\n\ndata: " + valid + "\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 7, len(tc.body)} {
				var d modelSSEDecoder
				got := ""
				for i := 0; i < len(tc.body); i += size {
					if model, ok := d.feed([]byte(tc.body[i:min(i+size, len(tc.body))])); ok {
						got = model
					}
				}
				if got != tc.want {
					t.Fatalf("chunk size %d: got %q, want %q", size, got, tc.want)
				}
			}
		})
	}
}

func TestModelScanRequestBudgetAndTTL(t *testing.T) {
	resetModelScans()
	t.Cleanup(resetModelScans)
	rememberModelScan("one", "account-one", "model-a", 780, false, "")
	rememberModelScan("two", "account-two", "model-a", 780, false, "")
	consumeModelScanChunk("one", []byte("data: {\"type\":\"response.created\","))
	entry, model, ok := consumeModelScanChunk("two", []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\",\"model\":\"model-b\"}}\n\n"))
	if !ok || model != "model-b" || entry.authID != "account-two" {
		t.Fatal("cross-request attribution failed")
	}
	consumeModelScanChunk("one", []byte(strings.Repeat("x", modelScanMaxBytes)))
	if _, ok := recallModelScan("one"); ok {
		t.Fatal("over-budget watch was retained")
	}
	rememberModelScan("expired", "account-one", "model-a", 780, false, "")
	pendingModelScans.mu.Lock()
	old := pendingModelScans.byID["expired"]
	old.seenAt = time.Now().Add(-pendingAuthTTL - time.Second)
	pendingModelScans.byID["expired"] = old
	pendingModelScans.mu.Unlock()
	if _, _, ok := consumeModelScanChunk("expired", []byte("data: {}\n\n")); ok {
		t.Fatal("expired watch accepted evidence")
	}
	if _, ok := recallModelScan("expired"); ok {
		t.Fatal("expired watch was retained")
	}
}
