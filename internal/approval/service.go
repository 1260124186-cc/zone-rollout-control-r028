package approval

import (
	"context"
	"sort"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store  *store.Store
	clock  platform.Clock
	ids    platform.IDGenerator
	policy domain.ApprovalPolicy
}

func New(
	stateStore *store.Store,
	clock platform.Clock,
	ids platform.IDGenerator,
	policy domain.ApprovalPolicy,
) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids, policy: policy}
}

// CheckActivation is the plan.ActivationGate implementation. It runs against
// the in-flight transaction state, so approval evidence and the plan version
// transition are decided under the same lock with no read-then-act window.
func (s *Service) CheckActivation(
	state *domain.State,
	plan domain.RolloutPlan,
	now time.Time,
) domain.ApprovalEvaluation {
	revision, err := currentRevision(state, plan)
	if err != nil {
		return domain.ApprovalEvaluation{
			Ready:        false,
			MissingRoles: append([]domain.ApprovalRole(nil), s.policy.RequiredRoles...),
			Reasons:      []string{domain.ApprovalReasonMissingRole},
		}
	}
	approvals := approvalsForPlan(state, plan.ID)
	return domain.EvaluateActivationApprovals(approvals, plan, revision, s.policy.RequiredRoles, now)
}

// Record binds a reviewer's decision to the exact current plan version and
// the current immutable revision. Nothing about the binding is accepted from
// the request body; it is computed inside the transaction.
func (s *Service) Record(
	ctx context.Context,
	planID string,
	input domain.ApprovalCreateInput,
) (domain.PlanApproval, error) {
	input = domain.NormalizeApprovalInput(input)
	if err := domain.ValidateApprovalInput(input); err != nil {
		return domain.PlanApproval{}, err
	}
	role := domain.ApprovalRole(input.Role)
	if !s.policy.RequiresRole(role) {
		return domain.PlanApproval{}, domain.Validation("approval role is not admitted by the active policy", map[string]any{
			"role":          input.Role,
			"allowed_roles": s.policy.RequiredRoles,
		})
	}
	decision := domain.ApprovalDecision(input.Decision)
	requestIndex := planID + "\x00" + input.RequestKey
	fingerprint := domain.ApprovalFingerprint(planID, input)
	now := s.clock.Now().UTC()
	var recorded domain.PlanApproval

	err := s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("approvals can be recorded only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		if existingID, exists := state.ApprovalRequestKeys[requestIndex]; exists {
			existing, found := state.Approvals[existingID]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "approval request index is inconsistent", map[string]any{
					"approval_id": existingID,
				})
			}
			if existing.Fingerprint != fingerprint {
				return domain.NewError(domain.CodeIdempotencyConflict, "request key was reused with different approval parameters", map[string]any{
					"request_key": input.RequestKey,
				})
			}
			recorded = existing
			return nil
		}
		revision, err := currentRevision(state, plan)
		if err != nil {
			return err
		}
		// One decision per role slot for this exact version+revision. A
		// second decision (by anyone) is a duplicate approval, not a second
		// signature; withdraw the prior one first. The same reviewer also
		// cannot hold a second role decision for the same version: dual
		// review requires two distinct identities.
		for _, approval := range state.Approvals {
			if approval.PlanID != planID || approval.Status != domain.ApprovalActive {
				continue
			}
			if approval.PlanVersion != plan.Version || approval.RevisionID != revision.ID {
				continue
			}
			if approval.Role == role {
				return domain.NewError(domain.CodeApprovalConflict, "this approval role already has an active decision for this version", map[string]any{
					"plan_id":      planID,
					"role":         role,
					"approval_id":  approval.ID,
					"plan_version": plan.Version,
				})
			}
			if approval.ReviewerID == input.ReviewerID {
				return domain.NewError(domain.CodeApprovalConflict, "reviewer already holds a decision in another role for this version", map[string]any{
					"plan_id":      planID,
					"reviewer_id":  input.ReviewerID,
					"approval_id":  approval.ID,
					"plan_version": plan.Version,
				})
			}
		}
		recorded = domain.PlanApproval{
			ID:                  s.ids.New("apr"),
			PlanID:              planID,
			PlanVersion:         plan.Version,
			RevisionID:          revision.ID,
			RevisionSerial:      revision.Serial,
			RevisionFingerprint: domain.RevisionDigest(*revision),
			Role:                role,
			ReviewerID:          input.ReviewerID,
			Decision:            decision,
			Note:                input.Note,
			RequestKey:          input.RequestKey,
			Fingerprint:         fingerprint,
			Status:              domain.ApprovalActive,
			CreatedAt:           now,
			ExpiresAt:           now.Add(s.policy.TTL),
		}
		state.Approvals[recorded.ID] = recorded
		state.ApprovalRequestKeys[requestIndex] = recorded.ID
		store.AddAudit(state, s.ids, "plan_approval", recorded.ID, string(decision), now, map[string]any{
			"plan_id":      planID,
			"role":         role,
			"reviewer_id":  input.ReviewerID,
			"plan_version": plan.Version,
			"revision_id":  revision.ID,
		})
		return nil
	})
	return recorded, err
}

// Withdraw revokes an approval decision. Only the original reviewer can
// withdraw it, and only while the plan is still draft.
func (s *Service) Withdraw(
	ctx context.Context,
	approvalID string,
	input domain.ApprovalWithdrawInput,
) (domain.PlanApproval, error) {
	input.ReviewerID = canonical(input.ReviewerID)
	if err := domain.ValidateApprovalWithdrawInput(input); err != nil {
		return domain.PlanApproval{}, err
	}
	now := s.clock.Now().UTC()
	var result domain.PlanApproval

	err := s.store.Write(ctx, func(state *domain.State) error {
		approval, ok := state.Approvals[approvalID]
		if !ok {
			return domain.NotFound("approval", approvalID)
		}
		plan, found := state.Plans[approval.PlanID]
		if !found {
			return domain.NewError(domain.CodeStorageFailure, "approval references a missing plan", map[string]any{
				"plan_id": approval.PlanID,
			})
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("approvals can be withdrawn only while plan is draft", map[string]any{
				"plan_id": plan.ID,
				"state":   plan.State,
			})
		}
		if approval.ReviewerID != input.ReviewerID {
			return domain.NewError(domain.CodeApprovalNotWithdrawn, "approval can be withdrawn only by its reviewer", map[string]any{
				"approval_id": approvalID,
				"reviewer_id": input.ReviewerID,
			})
		}
		if approval.Status != domain.ApprovalActive {
			return domain.NewError(domain.CodeApprovalNotWithdrawn, "approval is already withdrawn", map[string]any{
				"approval_id": approvalID,
				"status":      approval.Status,
			})
		}
		withdrawnAt := now
		approval.Status = domain.ApprovalWithdrawn
		approval.WithdrawnAt = &withdrawnAt
		state.Approvals[approvalID] = approval
		store.AddAudit(state, s.ids, "plan_approval", approvalID, "withdrawn", now, map[string]any{
			"plan_id": plan.ID,
			"role":    approval.Role,
		})
		result = approval
		return nil
	})
	return result, err
}

func (s *Service) Get(ctx context.Context, approvalID string) (domain.ApprovalView, error) {
	now := s.clock.Now().UTC()
	var view domain.ApprovalView
	err := s.store.Read(ctx, func(state *domain.State) error {
		approval, ok := state.Approvals[approvalID]
		if !ok {
			return domain.NotFound("approval", approvalID)
		}
		plan, found := state.Plans[approval.PlanID]
		if !found {
			return domain.NewError(domain.CodeStorageFailure, "approval references a missing plan", map[string]any{
				"plan_id": approval.PlanID,
			})
		}
		revision, _ := currentRevision(state, plan)
		view = singleView(approval, plan, revision, now)
		return nil
	})
	return view, err
}

// SummaryForPlan evaluates the whole ledger for one plan at the current time.
func (s *Service) SummaryForPlan(ctx context.Context, planID string) (domain.ApprovalSummary, error) {
	now := s.clock.Now().UTC()
	var summary domain.ApprovalSummary
	err := s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		revision, _ := currentRevision(state, plan)
		summary = s.buildSummary(state, plan, revision, now)
		return nil
	})
	return summary, err
}

func (s *Service) buildSummary(
	state *domain.State,
	plan domain.RolloutPlan,
	revision *domain.ZoneRevision,
	now time.Time,
) domain.ApprovalSummary {
	all := approvalsForPlan(state, plan.ID)
	views := domain.BuildApprovalViews(all, plan, revision, now)
	summary := domain.ApprovalSummary{
		PlanID:      plan.ID,
		PlanVersion: plan.Version,
		RevisionID:  plan.LatestRevisionID,
		Required:    plan.RequiresDualApproval,
		Approvals:   views,
	}
	if plan.RequiresDualApproval {
		summary.RequiredRoles = append([]domain.ApprovalRole(nil), s.policy.RequiredRoles...)
		rejections := make([]string, 0)
		for _, view := range views {
			if view.Effective && view.Decision == domain.DecisionRejected {
				rejections = append(rejections, view.ID)
			}
		}
		sort.Strings(rejections)
		summary.ActiveRejections = rejections
		evaluation := domain.EvaluateActivationApprovals(all, plan, revision, s.policy.RequiredRoles, now)
		summary.Ready = evaluation.Ready
		summary.MissingRoles = evaluation.MissingRoles
		summary.Reasons = evaluation.Reasons
	}
	return summary
}

func singleView(
	approval domain.PlanApproval,
	plan domain.RolloutPlan,
	revision *domain.ZoneRevision,
	now time.Time,
) domain.ApprovalView {
	validity := domain.EvaluateApproval(approval, plan, revision, now)
	return domain.ApprovalView{
		PlanApproval:    approval,
		Current:         validity.Current,
		Effective:       validity.Effective,
		IneffectiveCode: validity.Code,
	}
}

func approvalsForPlan(state *domain.State, planID string) []domain.PlanApproval {
	result := make([]domain.PlanApproval, 0)
	for _, approval := range state.Approvals {
		if approval.PlanID == planID {
			result = append(result, approval)
		}
	}
	return result
}

func currentRevision(state *domain.State, plan domain.RolloutPlan) (*domain.ZoneRevision, error) {
	if plan.LatestRevisionID == "" {
		return nil, domain.InvalidState("plan needs a zone revision before approval", map[string]any{
			"plan_id": plan.ID,
		})
	}
	revision, ok := state.Revisions[plan.LatestRevisionID]
	if !ok {
		return nil, domain.NewError(domain.CodeStorageFailure, "plan references a missing revision", map[string]any{
			"revision_id": plan.LatestRevisionID,
		})
	}
	return &revision, nil
}

func canonical(value string) string {
	input := domain.ApprovalCreateInput{ReviewerID: value}
	return domain.NormalizeApprovalInput(input).ReviewerID
}
