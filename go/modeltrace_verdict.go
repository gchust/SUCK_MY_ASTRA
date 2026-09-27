package main

import (
	"fmt"
	"math"
	"strings"
)

const (
	modeltraceMatch     = "fingerprint_match"
	modeltraceMismatch  = "fingerprint_mismatch"
	modeltraceUncertain = "uncertain"
	modeltraceFailed    = "probe_failed"
)

// Temperature calibration is not a rejection policy. No production threshold
// is invented here: an independently validated profile must accompany the bank.
type modeltraceDecisionPolicy struct {
	ValidationID    string  `json:"validation_id"`
	MinValidOutputs int     `json:"min_valid_outputs"`
	MinConfidence   float64 `json:"min_confidence"`
	MinMargin       float64 `json:"min_margin"`
}

func (p *modeltraceDecisionPolicy) valid() bool {
	return p != nil && strings.TrimSpace(p.ValidationID) != "" && p.MinValidOutputs > 0 &&
		p.MinConfidence > 0 && p.MinConfidence <= 1 && !math.IsNaN(p.MinConfidence) &&
		p.MinMargin > 0 && p.MinMargin <= 1 && !math.IsNaN(p.MinMargin)
}

func modeltraceVerdict(att *attributionReport, bank *modeltraceBank, requested string, planned int) (string, string, float64) {
	if att == nil || att.ValidOutputs == 0 {
		return modeltraceFailed, "no_valid_completed_outputs", 0
	}
	known := false
	for _, m := range bank.Models {
		known = known || m.ID == requested
	}
	if !known {
		return modeltraceUncertain, "requested_model_not_in_reference_bank", 0
	}
	if planned <= 0 || att.ValidOutputs != planned {
		return modeltraceUncertain, "incomplete_probe_set", 0
	}
	minimum := bank.RecommendedQueries
	if minimum <= 0 {
		return modeltraceUncertain, "missing_reference_sample_requirement", 0
	}
	if att.ValidOutputs < minimum {
		return modeltraceUncertain, "insufficient_valid_outputs", 0
	}
	if len(att.Results) < 2 || math.IsNaN(att.Confidence) || math.IsInf(att.Confidence, 0) {
		return modeltraceUncertain, "invalid_candidate_scores", 0
	}
	margin := att.Results[0].Probability - att.Results[1].Probability
	if math.IsNaN(margin) || math.IsInf(margin, 0) || margin <= 0 {
		return modeltraceUncertain, "ambiguous_candidates", 0
	}
	if len(att.PerOutputModels) != att.ValidOutputs {
		return modeltraceUncertain, "missing_per_turn_predictions", margin
	}
	for _, m := range att.PerOutputModels {
		if m != att.PredictedModel {
			return modeltraceUncertain, "inconsistent_per_turn_predictions", margin
		}
	}
	p := bank.DecisionPolicy
	if !p.valid() {
		return modeltraceUncertain, "no_validated_decision_policy", margin
	}
	if att.ValidOutputs < p.MinValidOutputs || att.Confidence < p.MinConfidence || margin < p.MinMargin {
		return modeltraceUncertain, "below_validated_decision_thresholds", margin
	}
	if att.PredictedModel == requested {
		return modeltraceMatch, "reference_fingerprint_agreement_not_identity_proof", margin
	}
	return modeltraceMismatch, "reference_fingerprint_disagreement_not_downgrade_proof", margin
}

func collectModeltraceTurn(ch modeltraceChallenge, served, output string, err error, outputs *[]string) modeltraceTurn {
	turn := modeltraceTurn{DeclaredServed: served, OutputLen: len(output), ExpectedNumbers: ch.ExpectedCount,
		ParsedNumbers: len(parseNumbers(output)), Status: "failed"}
	if err != nil {
		turn.Error = err.Error()
		return turn
	}
	if strings.TrimSpace(served) == "" {
		turn.Error = "missing response model declaration"
		return turn
	}
	if ch.ExpectedCount < bankMinNumbers || turn.ParsedNumbers != ch.ExpectedCount {
		turn.Status = "invalid_output"
		turn.Error = fmt.Sprintf("expected %d numbers, parsed %d", ch.ExpectedCount, turn.ParsedNumbers)
		return turn
	}
	turn.Status = "completed"
	*outputs = append(*outputs, output)
	return turn
}
