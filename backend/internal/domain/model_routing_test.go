package domain

import (
	"math"
	"testing"
)

func TestModelRoutingRejectsInvalidQuotaMeasurements(t *testing.T) {
	for _, used := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		limits := AgentAccountLimits{Windows: []AgentAccountLimitWindow{{UsedPercent: used}}}
		if _, known := limits.HandoffHeadroom(AgentAccountProviderCodex, "model"); known {
			t.Fatal("invalid quota accepted")
		}
	}
}

func TestModelRoutingValidation(t *testing.T) {
	valid := ModelRoute{Agent: HarnessClaudeCode, Model: "claude-sonnet-5-5", Effort: "medium", Level: 1}
	for _, tc := range []struct {
		name   string
		mutate func(*ModelRoutingConfig)
	}{
		{"unknown agent", func(p *ModelRoutingConfig) { p.Workers[0].Agent = HarnessGemini }},
		{"invalid level", func(p *ModelRoutingConfig) { p.Workers[0].Level = 0 }},
		{"injected model", func(p *ModelRoutingConfig) { p.Workers[0].Model = "model\nother" }},
		{"unbounded retries", func(p *ModelRoutingConfig) { p.MaxSwitches = 20 }},
		{"invalid reserve", func(p *ModelRoutingConfig) { p.ReservePercent = 101 }},
		{"unknown effort", func(p *ModelRoutingConfig) { p.Workers[0].Effort = "impossible" }},
		{"duplicate", func(p *ModelRoutingConfig) { p.Workers = append(p.Workers, p.Workers[0]) }},
		{"empty allowlist", func(p *ModelRoutingConfig) { p.Workers = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ModelRoutingConfig{Enabled: true, Workers: []ModelRoute{valid}}
			tc.mutate(&p)
			if err := (ProjectConfig{ModelRouting: &p}).Validate(); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	p := ModelRoutingConfig{Enabled: true, Workers: []ModelRoute{valid}}
	if err := p.Validate(); err != nil || p.SwitchLimit() != 2 {
		t.Fatalf("valid policy: %v", err)
	}
	if !valid.MatchesModel("sonnet") || valid.MatchesModel("opus") || valid.MatchesModel("sonnet-fake") {
		t.Fatal("incorrect family alias matching")
	}
}
