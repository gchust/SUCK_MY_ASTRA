package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func safetyWSFrame(opcode byte, payload string) []byte {
	head := []byte{0x80 | opcode}
	if len(payload) < 126 {
		head = append(head, byte(len(payload)))
	} else {
		head = append(head, 126, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(len(payload)))
	}
	return append(head, []byte(payload)...)
}

func TestWSProbeRequiresCorrelatedCompletion(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r1","model":"model-a"}}`
	delta := `{"type":"response.output_text.delta","response_id":"r1","delta":"1, 2, 3"}`
	completed := `{"type":"response.completed","response":{"id":"r1","model":"model-a","status":"completed"}}`
	for _, tc := range []struct {
		name   string
		events []string
		close  bool
		wantOK bool
	}{
		{"completed", []string{created, delta, completed}, false, true},
		{"eof_after_output", []string{created, delta}, false, false},
		{"close_after_output", []string{created, delta}, true, false},
		{"incomplete", []string{created, delta, `{"type":"response.incomplete","response":{"id":"r1"}}`}, false, false},
		{"failed", []string{created, delta, `{"type":"response.failed","response":{"id":"r1"}}`}, false, false},
		{"missing_created", []string{delta, completed}, false, false},
		{"missing_created_id", []string{`{"type":"response.created","response":{"model":"model-a"}}`, delta, completed}, false, false},
		{"wrong_id", []string{created, delta, strings.ReplaceAll(completed, "r1", "r2")}, false, false},
		{"wrong_model", []string{created, delta, strings.ReplaceAll(completed, "model-a", "model-b")}, false, false},
		{"invalid_json", []string{created, "broken"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames []byte
			for _, e := range tc.events {
				frames = append(frames, safetyWSFrame(1, e)...)
			}
			if tc.close {
				frames = append(frames, safetyWSFrame(8, "")...)
			}
			model, output, err := wsCollectOutput(nil, bufio.NewReader(bytes.NewReader(frames)))
			if (err == nil) != tc.wantOK {
				t.Fatalf("unexpected completion: model=%q output=%q err=%v", model, output, err)
			}
			if tc.wantOK && (model != "model-a" || output != "1, 2, 3") {
				t.Fatal("completed output lost")
			}
		})
	}
}

func TestFailedOrShortSamplesNeverEnterFingerprint(t *testing.T) {
	ch := modeltraceChallenge{ExpectedCount: 90}
	text := strings.Repeat("17, ", 90)
	var outputs []string
	failed := collectModeltraceTurn(ch, "model-a", text, errors.New("truncated"), &outputs)
	if failed.Status != "failed" || len(outputs) != 0 {
		t.Fatal("failed sample admitted")
	}
	short := collectModeltraceTurn(ch, "model-a", strings.Repeat("17, ", 80), nil, &outputs)
	if short.Status != "invalid_output" || len(outputs) != 0 {
		t.Fatal("short sample admitted")
	}
	good := collectModeltraceTurn(ch, "model-a", text, nil, &outputs)
	if good.Status != "completed" || len(outputs) != 1 {
		t.Fatal("complete sample rejected")
	}
}

func TestClientProbeCannotPretendToPinAccount(t *testing.T) {
	_, err := runModeltraceProbe(context.Background(), pluginConfig{}, "model-a", "fc", "", "account-a", "client", "", "", "", 3)
	if err == nil || !strings.Contains(err.Error(), "cannot pin") {
		t.Fatalf("account selection was not rejected before I/O: %v", err)
	}
}

func TestRelayGradeRequiresCompletionWitness(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"complete", `{"completed":true,"response_id":"r1","served":"model-a","output_text":"17","reason":"ok"}`, true},
		{"legacy_missing_witness", `{"served":"model-a","output_text":"17","reason":"ok"}`, false},
		{"incomplete", `{"completed":false,"response_id":"r1","served":"model-a","output_text":"17","reason":"ok"}`, false},
		{"missing_identity", `{"completed":true,"served":"model-a","output_text":"17","reason":"ok"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := cloudGradeTurn(context.Background(), srv.URL, "fixture-key", cloudMintCredentials{AccessToken: "fixture-token"}, "model-a", "fixture-state", "", "fixture-prompt", "", "")
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected completion witness acceptance: %v", err)
			}
		})
	}
}

func TestFingerprintVerdictGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*attributionReport, *modeltraceBank, *string, *int)
		want   string
	}{
		{"match", func(*attributionReport, *modeltraceBank, *string, *int) {}, modeltraceMatch},
		{"no_policy", func(_ *attributionReport, b *modeltraceBank, _ *string, _ *int) { b.DecisionPolicy = nil }, modeltraceUncertain},
		{"missing_validation", func(_ *attributionReport, b *modeltraceBank, _ *string, _ *int) { b.DecisionPolicy.ValidationID = "" }, modeltraceUncertain},
		{"missing_round", func(a *attributionReport, _ *modeltraceBank, _ *string, _ *int) { a.ValidOutputs = 2 }, modeltraceUncertain},
		{"too_few_rounds", func(a *attributionReport, _ *modeltraceBank, _ *string, n *int) { a.ValidOutputs = 1; *n = 1 }, modeltraceUncertain},
		{"unknown_model", func(_ *attributionReport, _ *modeltraceBank, m *string, _ *int) { *m = "not-in-bank" }, modeltraceUncertain},
		{"low_confidence", func(a *attributionReport, _ *modeltraceBank, _ *string, _ *int) { a.Confidence = .6 }, modeltraceUncertain},
		{"small_margin", func(a *attributionReport, _ *modeltraceBank, _ *string, _ *int) { a.Results[1].Probability = .96 }, modeltraceUncertain},
		{"disagreeing_turns", func(a *attributionReport, _ *modeltraceBank, _ *string, _ *int) { a.PerOutputModels[1] = "model-b" }, modeltraceUncertain},
		{"mismatch", func(_ *attributionReport, _ *modeltraceBank, m *string, _ *int) { *m = "model-b" }, modeltraceMismatch},
		{"no_output", func(a *attributionReport, _ *modeltraceBank, _ *string, _ *int) { a.ValidOutputs = 0 }, modeltraceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// These thresholds are synthetic fixture values, not production policy.
			bank := &modeltraceBank{RecommendedQueries: 3, Models: []bankModel{{ID: "model-a"}, {ID: "model-b"}}, DecisionPolicy: &modeltraceDecisionPolicy{ValidationID: "synthetic-test-only", MinValidOutputs: 3, MinConfidence: .9, MinMargin: .2}}
			att := &attributionReport{PredictedModel: "model-a", Confidence: .99, ValidOutputs: 3, PerOutputModels: []string{"model-a", "model-a", "model-a"}, Results: []modelAttribution{{Model: "model-a", Probability: .99}, {Model: "model-b", Probability: .01}}}
			model, planned := "model-a", 3
			tc.change(att, bank, &model, &planned)
			got, reason, _ := modeltraceVerdict(att, bank, model, planned)
			if got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}

func TestUncalibratedFingerprintIsNeverFullStrength(t *testing.T) {
	text := strings.Repeat("17, ", 300)
	report := modeltraceReport{RequestedTurns: 3}
	report.applyFingerprint([]string{text, text, text}, "gpt-6-astra")
	if report.Fingerprint == nil || report.Verdict != modeltraceUncertain || report.Fingerprint.Match {
		t.Fatalf("uncalibrated result %+v", report)
	}
	report.applyFingerprint(nil, "gpt-6-astra")
	if report.Fingerprint != nil || report.Verdict != modeltraceFailed {
		t.Fatal("stale result survived failed probe")
	}
}
