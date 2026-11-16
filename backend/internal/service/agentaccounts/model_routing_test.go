package agentaccounts

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestModelRoutingLimitsUsesPinnedOrLaunchDefaultOnly(t *testing.T) {
	for _, bound := range []bool{true, false} {
		svc, store, provider := sessionLimitsFixture(t)
		if !bound {
			delete(store.bindings, "session")
		}
		if _, err := svc.RoutingLimits(context.Background(), "session", domain.HarnessClaudeCode); err != nil {
			t.Fatal(err)
		}
		want := "/account-b"
		if !bound {
			want = "/account-a"
		}
		if len(provider.homes) != 1 || provider.homes[0] != want {
			t.Fatalf("queried %v, want %s", provider.homes, want)
		}
	}
}

func TestModelRoutingLimitsRefusesMissingBindingAndFailedProbe(t *testing.T) {
	for _, failure := range []string{"missing", "signed-out", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			svc, store, provider := sessionLimitsFixture(t)
			switch failure {
			case "missing":
				store.accounts = store.accounts[:1]
			case "signed-out":
				delete(provider.signedInAs, "/account-b")
			case "unavailable":
				provider.read = func(context.Context, string) (domain.AgentAccountLimits, error) {
					return domain.AgentAccountLimits{}, errors.New("offline")
				}
			}
			if _, err := svc.RoutingLimits(context.Background(), "session", domain.HarnessClaudeCode); err == nil {
				t.Fatal("unknown capacity accepted")
			}
		})
	}
}
