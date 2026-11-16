package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ModelRoutingProjects reads the saved, user-approved per-project allowlist.
type ModelRoutingProjects interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}

// ModelRoutingAccounts checks the account the target would actually use.
type ModelRoutingAccounts interface {
	RoutingLimits(context.Context, domain.SessionID, domain.AgentHarness) (domain.AgentAccountLimits, error)
}

// ModelRoutingCatalog excludes invented, unavailable and unsupported model settings.
type ModelRoutingCatalog interface {
	Models(context.Context, string, string, bool) (ports.AgentModelCatalog, error)
}

type modelRoutingDeps struct {
	projects ModelRoutingProjects
	accounts ModelRoutingAccounts
	models   ModelRoutingCatalog
}

type modelRoutingNote struct {
	Event  string            `json:"event"`
	State  string            `json:"state"`
	Target domain.ModelRoute `json:"target"`
	Reason string            `json:"reason,omitempty"`
}

// SetModelRouting 在啟動背景監看前接線，切換不依賴指揮官還能不能回答。
func (s *Service) SetModelRouting(projects ModelRoutingProjects, accounts ModelRoutingAccounts, models ModelRoutingCatalog) {
	if projects != nil && accounts != nil && models != nil {
		s.modelRouting = &modelRoutingDeps{projects: projects, accounts: accounts, models: models}
	}
}

func (s *Service) routingPolicy(ctx context.Context, record domain.SessionRecord) *domain.ModelRoutingConfig {
	if s.modelRouting == nil || record.ProjectID == "" || domain.NormalizeSessionMode(record.Mode) != domain.SessionModeChat {
		return nil
	}
	project, found, err := s.modelRouting.projects.GetProject(ctx, string(record.ProjectID))
	if err != nil || !found || !project.ArchivedAt.IsZero() || project.Config.ModelRouting == nil {
		return nil
	}
	p := project.Config.ModelRouting
	if !p.Enabled || p.Validate() != nil {
		return nil
	}
	return p
}

// routingBudget counts the durable retry links, so restarting cannot reset the cap.
func (s *Service) routingBudget(ctx context.Context, source domain.ConversationTurn, limit int) (bool, error) {
	for count := 0; source.RetryOfTurnID != ""; count++ {
		if count+1 >= limit {
			return false, nil
		}
		parent, err := s.store.TurnByID(ctx, source.RetryOfTurnID)
		if err != nil {
			return false, err
		}
		if parent.ConversationID != source.ConversationID || parent.ID == source.ID {
			return false, errors.New("invalid retry lineage")
		}
		source = parent
	}
	return true, nil
}

// RouteSpawn 讓指揮官只需判斷任務難度；實際模型與額度由背景程式選擇。
func (s *Service) RouteSpawn(ctx context.Context, cfg ports.SpawnConfig) (ports.SpawnConfig, error) {
	if cfg.RoutingLevel < 1 || cfg.RoutingLevel > 3 || cfg.ProjectID == "" || cfg.AgentAccountID != "" || cfg.AgentConfig.Model != "" || cfg.AgentConfig.Effort != "" || cfg.RequestedMode == domain.SessionModeTUI {
		return cfg, apierr.Invalid("MODEL_ROUTING_INVALID", "自動選模需要專案、Chat 介面與 1 到 3 的級別；不可同時指定模型、推理強度或帳號。", nil)
	}
	record := domain.SessionRecord{ProjectID: cfg.ProjectID, Kind: cfg.Kind, Mode: domain.SessionModeChat}
	policy := s.routingPolicy(ctx, record)
	if policy == nil {
		return cfg, apierr.Conflict("MODEL_ROUTING_DISABLED", "專案尚未開啟自動選模。", nil)
	}
	routes := policy.Workers
	if cfg.Kind == domain.KindOrchestrator {
		routes = policy.Orchestrators
	}
	chosen := s.pickModelRoute(ctx, record, routes, cfg.RoutingLevel, "")
	if chosen == nil {
		return cfg, apierr.Conflict("MODEL_ROUTING_NO_CAPACITY", "沒有已確認額度足夠且符合任務級別的模型，已停止派工。", nil)
	}
	cfg.Harness, cfg.AgentConfig.Model, cfg.AgentConfig.Effort = chosen.Agent, chosen.Model, chosen.Effort
	cfg.EffortOverride = true
	cfg.RequestedMode = domain.SessionModeChat
	return cfg, nil
}

func (s *Service) routeUsageLimitedTurn(ctx context.Context, h *accountHandoff, source domain.ConversationTurn, record domain.SessionRecord, policy *domain.ModelRoutingConfig) (bool, error) {
	if s.reader == nil {
		return true, nil
	}
	rows, err := s.reader.LoadConversationSnapshot(ctx, source.ConversationID)
	if err != nil {
		return s.giveUpAfter(h, err)
	}
	conversation, err := s.store.ConversationForSession(ctx, record.ID)
	if err != nil {
		return s.giveUpAfter(h, err)
	}
	// 先讀取已保存的切換紀錄，重啟後不會再選一次、反覆來回換模型。
	for _, activity := range rows.Activities {
		if activity.ProviderItemID != "model-routing:"+source.ID {
			continue
		}
		var note modelRoutingNote
		if err := json.Unmarshal(activity.Detail, &note); err != nil {
			return s.stopModelRouting(ctx, h, source, "上次模型切換紀錄無法讀取，已停止自動重試。")
		}
		if note.Event != "model.limit_routing" {
			return true, nil
		}
		if note.State != "selected" {
			return true, nil
		}
		if record.Harness != note.Target.Agent || !note.Target.MatchesModel(conversation.Settings.Model) || conversation.Settings.ReasoningEffort != note.Target.Effort {
			return s.stopModelRouting(ctx, h, source, "上次切換未完成，請檢查目前模型後再繼續。")
		}
		return s.retryRoutedTurn(ctx, h, source, note.Target)
	}
	ok, err := s.routingBudget(ctx, source, policy.SwitchLimit())
	if err != nil {
		return s.giveUpAfter(h, err)
	}
	if !ok {
		return s.stopModelRouting(ctx, h, source, "已達自動切換次數上限，請確認額度後再繼續。")
	}
	routes := policy.Workers
	if record.Kind == domain.KindOrchestrator {
		routes = policy.Orchestrators
	}
	model := conversation.Settings.Model
	if model == "" {
		model = record.Metadata.Model
	}
	level := 0
	for _, r := range routes {
		if r.Agent == record.Harness && r.MatchesModel(model) && (r.Effort == conversation.Settings.ReasoningEffort || conversation.Settings.ReasoningEffort == "") {
			level = max(level, r.Level)
		}
	}
	if level == 0 {
		return s.stopModelRouting(ctx, h, source, "目前模型未列入允許清單，無法安全判定接手級別。")
	}
	chosen := s.pickModelRoute(ctx, record, routes, level, model)
	if chosen == nil {
		return s.stopModelRouting(ctx, h, source, "沒有額度足夠且符合目前能力級別的模型；已停止自動重試。")
	}
	// 人工送出新訊息、出現待核准事項或關閉設定時，不可偷偷切換。
	fresh := s.routingPolicy(ctx, record)
	if fresh == nil {
		return true, nil
	}
	freshRoutes := fresh.Workers
	if record.Kind == domain.KindOrchestrator {
		freshRoutes = fresh.Orchestrators
	}
	if !slices.Contains(freshRoutes, *chosen) {
		return true, nil
	}
	if err := actionableUsageLimitTurn(ctx, s.store, source); err != nil {
		if permanentUsageLimitResumeError(err) {
			return true, nil
		}
		return s.giveUpAfter(h, err)
	}
	if chosen.Agent == record.Harness {
		return s.routeOnSameProvider(ctx, h, source, *chosen)
	}
	if err := s.writeModelRoutingNote(ctx, source, "selected", *chosen, ""); err != nil {
		return s.giveUpAfter(h, err)
	}
	request := domain.ProviderSwitchRequest{TargetHarness: chosen.Agent, Model: chosen.Model, Effort: chosen.Effort, IdempotencyKey: "model-routing:" + source.ID}
	if err := s.SwitchProvider(ctx, record.ID, request); err != nil {
		return s.stopModelRouting(ctx, h, source, "模型接手未完成，已保留原進度並停止自動重試。")
	}
	return s.retryRoutedTurn(ctx, h, source, *chosen)
}

func (s *Service) pickModelRoute(ctx context.Context, record domain.SessionRecord, routes []domain.ModelRoute, level int, skipModel string) *domain.ModelRoute {
	policy := s.routingPolicy(ctx, record)
	if policy == nil {
		return nil
	}
	// 每家只查一次最新額度；整個帳號與模型專用上限都要有餘量。
	limits := map[domain.AgentHarness]domain.AgentAccountLimits{}
	catalogs := map[domain.AgentHarness]ports.AgentModelCatalog{}
	checked := map[domain.AgentHarness]bool{}
	for _, r := range routes {
		if r.Level < level || skipModel != "" && r.Agent == record.Harness && r.MatchesModel(skipModel) {
			continue
		}
		if !checked[r.Agent] {
			checked[r.Agent] = true
			catalog, catErr := s.modelRouting.models.Models(ctx, string(r.Agent), "", false)
			reading, limitErr := s.modelRouting.accounts.RoutingLimits(ctx, record.ID, r.Agent)
			if catErr == nil && limitErr == nil && !catalog.Stale && catalog.WarningCode == "" {
				catalogs[r.Agent] = catalog
				limits[r.Agent] = reading
			}
		}
		catalog, known := catalogs[r.Agent]
		if !known || !routingModelOffered(catalog, r) {
			continue
		}
		provider, _ := domain.AgentAccountProviderFor(r.Agent)
		reading := limits[r.Agent]
		room, known := reading.HandoffHeadroom(provider, r.Model)
		if !known || math.IsNaN(room) || math.IsInf(room, 0) || room <= policy.ReservePercent {
			continue
		}
		return &r
	}
	return nil
}

func (s *Service) routeOnSameProvider(ctx context.Context, h *accountHandoff, source domain.ConversationTurn, target domain.ModelRoute) (bool, error) {
	controller, release, err := s.workingController(ctx, h.sessionID)
	if err != nil {
		return s.giveUpAfter(h, err)
	}
	defer release()
	if !controller.sendMu.TryLock() {
		return true, nil
	}
	defer controller.sendMu.Unlock()
	if controller.busy() {
		return true, nil
	}
	if err := actionableUsageLimitTurn(ctx, s.store, source); err != nil {
		if permanentUsageLimitResumeError(err) {
			return true, nil
		}
		return s.giveUpAfter(h, err)
	}
	if pending, err := s.store.HasPendingConversationInteractions(ctx, source.ConversationID); err != nil || pending {
		return true, err
	}
	if err := s.writeModelRoutingNote(ctx, source, "selected", target, ""); err != nil {
		return s.giveUpAfter(h, err)
	}
	settings := controller.Settings()
	settings.Model, settings.ReasoningEffort = target.Model, target.Effort
	if _, err := s.setTurnSettings(ctx, controller, domain.SessionConversationOwner(h.sessionID), settings); err != nil {
		return s.stopModelRouting(ctx, h, source, "無法設定接手模型，已停止自動重試。")
	}
	if _, err := controller.retryTurnLocked(ctx, source.ID, false); err != nil {
		return s.stopModelRouting(ctx, h, source, "已切換模型；原提示未重新送出，請檢查對話後繼續。")
	}
	return true, s.writeModelRoutingNote(ctx, source, "sent", target, "")
}

func routingModelOffered(catalog ports.AgentModelCatalog, r domain.ModelRoute) bool {
	return slices.ContainsFunc(catalog.Models, func(m ports.AgentModelInfo) bool {
		return m.ID == r.Model && m.RequiresVersion == "" && m.UnavailableReason == "" && (r.Effort == "" || slices.Contains(m.Efforts, r.Effort))
	})
}

func (s *Service) retryRoutedTurn(ctx context.Context, h *accountHandoff, source domain.ConversationTurn, target domain.ModelRoute) (bool, error) {
	// 沿用既有重試鎖與永久紀錄：保留原提示、附件、分支及檔案，不重做派工。
	err := s.retryAccountHandoffOnController(ctx, h)
	if err != nil {
		return s.stopModelRouting(ctx, h, source, "已切換模型；原提示未重新送出，請檢查對話後繼續。")
	}
	return true, s.writeModelRoutingNote(ctx, source, "sent", target, "")
}

func (s *Service) stopModelRouting(ctx context.Context, h *accountHandoff, source domain.ConversationTurn, reason string) (bool, error) {
	// 額度不足時停止，不會自行啟用額外付費、購買重置額度或無限續跑。
	if err := s.writeModelRoutingNote(ctx, source, "stopped", domain.ModelRoute{}, reason); err != nil {
		return s.giveUpAfter(h, err)
	}
	return true, nil
}

func (s *Service) writeModelRoutingNote(ctx context.Context, source domain.ConversationTurn, state string, target domain.ModelRoute, reason string) error {
	detail, err := json.Marshal(modelRoutingNote{Event: "model.limit_routing", State: state, Target: target, Reason: reason})
	if err != nil {
		return err
	}
	summary := reason
	if state == "selected" {
		summary = fmt.Sprintf("額度不足，準備由 %s / %s 接手。", target.Agent, target.Model)
	}
	if state == "sent" {
		summary = fmt.Sprintf("額度不足，已改由 %s / %s 接手並重新送出原提示。", target.Agent, target.Model)
	}
	return s.store.UpsertActivity(ctx, source.ConversationID, source.ProviderTurnID, domain.ConversationActivity{
		ID: s.newID(), Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Summary: summary, Detail: detail, ProviderItemID: "model-routing:" + source.ID,
	}, s.now())
}
