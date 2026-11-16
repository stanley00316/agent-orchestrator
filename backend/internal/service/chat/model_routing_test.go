package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type routingFacts struct {
	catalogs     map[domain.AgentHarness]ports.AgentModelCatalog
	limits       map[domain.AgentHarness]domain.AgentAccountLimits
	fail         bool
	beforeLimits func()
}

func (f *routingFacts) Models(_ context.Context, agent, _ string, _ bool) (ports.AgentModelCatalog, error) {
	return f.catalogs[domain.AgentHarness(agent)], nil
}
func (f *routingFacts) RoutingLimits(_ context.Context, _ domain.SessionID, agent domain.AgentHarness) (domain.AgentAccountLimits, error) {
	if f.beforeLimits != nil {
		f.beforeLimits()
	}
	if f.fail {
		return domain.AgentAccountLimits{}, errors.New("usage unavailable")
	}
	return f.limits[agent], nil
}

func wireRouting(t *testing.T, hh *handoffHarness, routes []domain.ModelRoute) *routingFacts {
	t.Helper()
	ctx := context.Background()
	project, _, err := hh.st.GetProject(ctx, string(testProject))
	if err != nil {
		t.Fatal(err)
	}
	project.Config.ModelRouting = &domain.ModelRoutingConfig{Enabled: true, Workers: routes, Orchestrators: routes, MaxSwitches: 2, ReservePercent: 10}
	if err := hh.st.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	f := &routingFacts{catalogs: map[domain.AgentHarness]ports.AgentModelCatalog{}, limits: map[domain.AgentHarness]domain.AgentAccountLimits{}}
	for _, r := range routes {
		cat := f.catalogs[r.Agent]
		cat.Models = append(cat.Models, ports.AgentModelInfo{ID: r.Model, Efforts: []string{r.Effort}})
		f.catalogs[r.Agent] = cat
		f.limits[r.Agent] = domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{ID: "shared", UsedPercent: 20}}}
	}
	hh.svc.SetModelRouting(hh.st, f, f)
	hh.accounts.target = ""
	if _, err := hh.svc.SetTurnSettings(ctx, testSession, domain.ConversationSettings{Model: routes[0].Model, ReasoningEffort: routes[0].Effort}); err != nil {
		t.Fatal(err)
	}
	return f
}

func routingRoutes() []domain.ModelRoute {
	return []domain.ModelRoute{
		{Agent: domain.HarnessCodex, Model: "source", Effort: "low", Level: 1},
		{Agent: domain.HarnessCodex, Model: "backup", Effort: "high", Level: 2},
		{Agent: domain.HarnessCodex, Model: "last", Effort: "high", Level: 3},
	}
}

func routingState(t *testing.T, h *harness, turn domain.ConversationTurn) string {
	t.Helper()
	rows, err := h.st.LoadConversationSnapshot(context.Background(), turn.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rows.Activities {
		if a.ProviderItemID == "model-routing:"+turn.ID {
			var note struct{ State string }
			if err := json.Unmarshal(a.Detail, &note); err != nil {
				t.Fatal(err)
			}
			return note.State
		}
	}
	return ""
}

func TestModelRoutingChangesModelAndRetriesOriginalOnce(t *testing.T) {
	hh := newHandoffHarness(t, domain.HarnessCodex)
	wireRouting(t, hh, routingRoutes())
	// Project opt-in works independently of the global account-only switch.
	hh.enabled.Store(false)
	source := failUsageLimitedTurn(t, hh.harness, timedUsageLimit)
	hh.tick(t)
	hh.tick(t)
	controller, err := hh.svc.Controller(testSession)
	if err != nil {
		t.Fatal(err)
	}
	if controller.Settings().Model != "backup" || len(hh.conv.sentMessages()) != 2 || hh.conv.sentMessages()[1].Text != "finish this task" || routingState(t, hh.harness, source) != "sent" {
		t.Fatal("model switch did not preserve and retry the original request")
	}
	if len(hh.mover.moves()) != 0 {
		t.Fatal("unexpected account move")
	}
}

func TestModelRoutingRetriesAfterTemporaryIntentReadFailure(t *testing.T) {
	h, wrapped := newUsageLimitReadHarness(t)
	hh := handoffHarnessFor(t, h, domain.HarnessCodex)
	facts := wireRouting(t, hh, routingRoutes())
	failUsageLimitedTurn(t, h, timedUsageLimit)
	facts.beforeLimits = func() { wrapped.failPrompt.Store(true) }
	err := hh.svc.HandoffUsageLimitedTurns(context.Background())
	if !errors.Is(err, errUsageLimitTemporaryRead) || len(hh.conv.sentMessages()) != 1 {
		t.Fatalf("temporary failure = %v, sends = %d", err, len(hh.conv.sentMessages()))
	}
	facts.beforeLimits = nil
	hh.advance(20 * time.Second)
	hh.tick(t)
	if len(hh.conv.sentMessages()) != 2 {
		t.Fatal("temporary read failure permanently consumed recovery")
	}
}

func TestModelRoutingReportsMalformedRecoveryNote(t *testing.T) {
	hh := newHandoffHarness(t, domain.HarnessCodex)
	wireRouting(t, hh, routingRoutes())
	source := failUsageLimitedTurn(t, hh.harness, timedUsageLimit)
	err := hh.st.UpsertActivity(context.Background(), source.ConversationID, source.ProviderTurnID, domain.ConversationActivity{
		ID: "invalid-routing-note", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted,
		Detail: []byte("[]"), ProviderItemID: "model-routing:" + source.ID,
	}, hh.now())
	if err != nil {
		t.Fatal(err)
	}
	hh.tick(t)
	if routingState(t, hh.harness, source) != "stopped" || len(hh.conv.sentMessages()) != 1 {
		t.Fatal("unreadable recovery note was not reported safely")
	}
}

func TestModelRoutingRefusesUnsafeCandidates(t *testing.T) {
	for _, failure := range []string{"unknown quota", "shared quota", "reserve", "model scope", "unknown model", "unsupported effort", "stale catalog", "no downgrade", "pending approval", "disabled"} {
		t.Run(failure, func(t *testing.T) {
			hh := newHandoffHarness(t, domain.HarnessCodex)
			routes := routingRoutes()[:2]
			f := wireRouting(t, hh, routes)
			source := failUsageLimitedTurn(t, hh.harness, timedUsageLimit)
			switch failure {
			case "unknown quota":
				f.fail = true
			case "shared quota":
				f.limits[domain.HarnessCodex] = domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{UsedPercent: 100}}}
			case "reserve":
				f.limits[domain.HarnessCodex] = domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{UsedPercent: 90}}}
			case "model scope":
				f.limits[domain.HarnessCodex] = domain.AgentAccountLimits{ReachedScopes: []string{"backup"}, Windows: []domain.AgentAccountLimitWindow{{UsedPercent: 0}}}
			case "unknown model":
				f.catalogs[domain.HarnessCodex] = ports.AgentModelCatalog{}
			case "unsupported effort":
				f.catalogs[domain.HarnessCodex] = ports.AgentModelCatalog{Models: []ports.AgentModelInfo{{ID: "backup", Efforts: []string{"low"}}}}
			case "stale catalog":
				cat := f.catalogs[domain.HarnessCodex]
				cat.Stale = true
				f.catalogs[domain.HarnessCodex] = cat
			case "no downgrade":
				project, _, _ := hh.st.GetProject(context.Background(), string(testProject))
				project.Config.ModelRouting.Workers[0].Level = 3
				project.Config.ModelRouting.Orchestrators[0].Level = 3
				if err := hh.st.UpsertProject(context.Background(), project); err != nil {
					t.Fatal(err)
				}
			case "pending approval":
				rec, _, _ := hh.st.GetSession(context.Background(), testSession)
				rec.Activity.State = domain.ActivityWaitingInput
				if err := hh.st.UpdateSession(context.Background(), rec); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				project, _, _ := hh.st.GetProject(context.Background(), string(testProject))
				project.Config.ModelRouting.Enabled = false
				if err := hh.st.UpsertProject(context.Background(), project); err != nil {
					t.Fatal(err)
				}
				hh.enabled.Store(false)
			}
			hh.tick(t)
			if len(hh.conv.sentMessages()) != 1 {
				t.Fatal("unsafe fallback dispatched")
			}
			controller, _ := hh.svc.Controller(testSession)
			if controller.Settings().Model != "source" {
				t.Fatal("unsafe fallback changed model")
			}
			if failure != "disabled" && failure != "pending approval" && routingState(t, hh.harness, source) != "stopped" {
				t.Fatal("missing stop report")
			}
		})
	}
}

func failRoutingRetry(t *testing.T, hh *handoffHarness, source domain.ConversationTurn) domain.ConversationTurn {
	t.Helper()
	id, found, err := hh.st.RetryTurnIDForSource(context.Background(), source.ConversationID, source.ID)
	if err != nil || !found {
		t.Fatal("missing retry")
	}
	retry, err := hh.st.TurnByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	hh.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: retry.ProviderTurnID, TurnState: domain.TurnStateFailed, Err: errors.New(timedUsageLimit)})
	rows := failedTurnSnapshot(t, hh.harness, retry.ID)
	failed, _ := turnByID(rows, retry.ID)
	return failed
}

func TestModelRoutingCapSurvivesRewiring(t *testing.T) {
	hh := newHandoffHarness(t, domain.HarnessCodex)
	wireRouting(t, hh, routingRoutes())
	source := failUsageLimitedTurn(t, hh.harness, timedUsageLimit)
	hh.tick(t)
	retry := failRoutingRetry(t, hh, source)
	hh.tick(t)
	last := failRoutingRetry(t, hh, retry)
	hh.wire(hh.svc) // Reset scanning as a restarted daemon does; durable links retain the cap.
	hh.tick(t)
	hh.tick(t)
	if len(hh.conv.sentMessages()) != 3 || routingState(t, hh.harness, last) != "stopped" {
		t.Fatal("retry cap was reset")
	}
}

func TestModelRoutingCrossProviderKeepsWorkspaceAndHandoff(t *testing.T) {
	h, _, driver := newEditHarness(t, false)
	hh := handoffHarnessFor(t, h, domain.HarnessCodex)
	routes := routingRoutes()[:1]
	routes = append(routes, domain.ModelRoute{Agent: domain.HarnessClaudeCode, Model: "claude-opus-5-5", Effort: "high", Level: 2})
	wireRouting(t, hh, routes)
	completed := completeTurn(t, h, "keep earlier work", "provider-turn-1")
	h.awaitSnapshot(t, func(rows store.ConversationSnapshot) bool {
		turn, ok := turnByID(rows, completed)
		return ok && turn.State == domain.TurnStateCompleted
	})
	before, _, _ := hh.st.GetSession(context.Background(), testSession)
	workspace := lastStart(driver).WorkspacePath
	if workspace == "" {
		t.Fatal("fixture has no workspace")
	}
	before.Metadata.WorkspacePath = workspace
	if err := hh.st.UpdateSession(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "work-in-progress.txt"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	source := failUsageLimitedTurn(t, h, timedUsageLimit)
	hh.tick(t)
	after, _, _ := hh.st.GetSession(context.Background(), testSession)
	if after.Harness != domain.HarnessClaudeCode || after.Metadata.WorkspacePath != workspace || routingState(t, h, source) != "sent" {
		t.Fatal("provider fallback did not preserve the session workspace")
	}
	if !strings.Contains(lastStart(driver).SystemPrompt, "keep earlier work") {
		t.Fatal("handoff lost earlier work context")
	}
	data, err := os.ReadFile(filepath.Join(workspace, "work-in-progress.txt"))
	if err != nil || string(data) != "keep me" {
		t.Fatal("working files changed")
	}
}

func TestModelRoutingSpawnSelectsCapacityWithoutStartingAgent(t *testing.T) {
	hh := newHandoffHarness(t, domain.HarnessCodex)
	f := wireRouting(t, hh, routingRoutes())
	for _, level := range []int{1, 2, 3} {
		cfg, err := hh.svc.RouteSpawn(context.Background(), ports.SpawnConfig{ProjectID: testProject, Kind: domain.KindWorker, RoutingLevel: level})
		if err != nil || cfg.AgentConfig.Model != routingRoutes()[level-1].Model || cfg.RequestedMode != domain.SessionModeChat {
			t.Fatalf("level %d -> %+v, %v", level, cfg, err)
		}
	}
	f.fail = true
	if _, err := hh.svc.RouteSpawn(context.Background(), ports.SpawnConfig{ProjectID: testProject, Kind: domain.KindWorker, RoutingLevel: 1}); err == nil {
		t.Fatal("spawn accepted unknown quota")
	}
	if len(hh.conv.sentMessages()) != 0 {
		t.Fatal("selection started a paid turn")
	}
}
