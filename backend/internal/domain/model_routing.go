package domain

import (
	"fmt"
	"math"
	"strings"
)

// ModelRoute 是使用者允許的模型與能力級別，不能由錯誤訊息任意指定。
type ModelRoute struct {
	Agent  AgentHarness `json:"agent"`
	Model  string       `json:"model"`
	Effort string       `json:"effort,omitempty"`
	Level  int          `json:"level" minimum:"1" maximum:"3"`
}

// ModelRoutingConfig 控制專案的自動接手；沒有設定的專案維持原本行為。
type ModelRoutingConfig struct {
	Enabled        bool         `json:"enabled"`
	Workers        []ModelRoute `json:"workers"`
	Orchestrators  []ModelRoute `json:"orchestrators,omitempty"`
	MaxSwitches    int          `json:"maxSwitches,omitempty" minimum:"1" maximum:"3"`
	ReservePercent float64      `json:"reservePercent,omitempty" minimum:"0" maximum:"50"`
}

// SwitchLimit limits retries across the durable retry lineage, including after restart.
func (c ModelRoutingConfig) SwitchLimit() int {
	if c.MaxSwitches == 0 {
		return 2
	}
	return c.MaxSwitches
}

// Validate refuses unbounded retries and unknown harnesses before saving a policy.
func (c ModelRoutingConfig) Validate() error {
	if c.MaxSwitches < 0 || c.MaxSwitches > 3 || math.IsNaN(c.ReservePercent) || math.IsInf(c.ReservePercent, 0) || c.ReservePercent < 0 || c.ReservePercent > 50 {
		return fmt.Errorf("modelRouting: maxSwitches must be 1..3 and reservePercent 0..50")
	}
	if c.Enabled && len(c.Workers) == 0 {
		return fmt.Errorf("modelRouting.workers: at least one allowed model is required")
	}
	for role, routes := range map[string][]ModelRoute{"workers": c.Workers, "orchestrators": c.Orchestrators} {
		if len(routes) > 12 {
			return fmt.Errorf("modelRouting.%s: at most 12 models are allowed", role)
		}
		seen := map[string]bool{}
		for _, r := range routes {
			if r.Agent != HarnessClaudeCode && r.Agent != HarnessCodex {
				return fmt.Errorf("modelRouting.%s: only claude-code and codex are supported", role)
			}
			if r.Level < 1 || r.Level > 3 || r.Model == "" || len(r.Model) > 160 || strings.TrimSpace(r.Model) != r.Model || strings.ContainsAny(r.Model, "\x00\r\n\t") {
				return fmt.Errorf("modelRouting.%s: invalid model or level", role)
			}
			if !validRoutingEffort(r.Effort) {
				return fmt.Errorf("modelRouting.%s: unsupported effort %q", role, r.Effort)
			}
			key := string(r.Agent) + ":" + r.Model + ":" + r.Effort
			if seen[key] {
				return fmt.Errorf("modelRouting.%s: duplicate model and effort", role)
			}
			seen[key] = true
		}
	}
	return nil
}

func validRoutingEffort(e string) bool {
	switch e {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}

// MatchesModel also accepts the native Claude family aliases reported by live chats.
func (r ModelRoute) MatchesModel(model string) bool {
	if r.Model == model {
		return true
	}
	if r.Agent == HarnessClaudeCode {
		switch model {
		case "opus", "sonnet", "fable", "haiku":
			return strings.HasPrefix(r.Model, "claude-"+model+"-")
		}
	}
	return false
}
