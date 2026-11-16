package session

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestModelRoutingRunsBeforeAgentLaunch(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		st := newFakeStore()
		st.projects["demo"] = domain.ProjectRecord{ID: "demo", Path: t.TempDir()}
		fc := &fakeCommander{spawnRecord: domain.SessionRecord{ID: "demo-1", ProjectID: "demo", Kind: domain.KindWorker, Harness: domain.HarnessCodex}}
		svc := &Service{manager: fc, store: st}
		svc.SetModelRouter(func(_ context.Context, cfg ports.SpawnConfig) (ports.SpawnConfig, error) {
			if refuse {
				return cfg, errors.New("unknown quota")
			}
			cfg.Harness = domain.HarnessCodex
			cfg.AgentConfig.Model = "allowed"
			return cfg, nil
		})
		_, _, _, err := svc.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "demo", Kind: domain.KindWorker, RoutingLevel: 1, Prompt: "Read only"})
		if refuse {
			if err == nil || fc.spawned {
				t.Fatal("agent launched without verified capacity")
			}
		} else if err != nil || fc.spawnedCfg.AgentConfig.Model != "allowed" {
			t.Fatalf("router was bypassed: %+v, %v", fc.spawnedCfg, err)
		}
	}
}
