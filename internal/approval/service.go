package approval

import (
	"context"
	"sort"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

// Record appends an approval entry bound to the plan's current revision, the
// revision's immutable content digest and the current plan version. None of
// those bindings can come from the request body: they are read from
// server-side state inside the serializing write transaction.
func (s *Service) Record(
	ctx context.Context,
	planID string,
	input domain.ApprovalCreateInput,
) (domain.PlanApproval, error) {
	input = domain.NormalizeApprovalInput(input)
	if err := domain.ValidateApprovalInput(input); err != nil {
		return domain.PlanApproval{}, err
	}
	fingerprint := domain.ApprovalRequestFingerprint(planID, input)
	requestIndex := planID + "\x00" + input.RequestKey
	var result domain.PlanApproval
	err := s.store.Write(ctx, func(state *domain.State) error {
		// Idempotency is resolved before any state rule so a retried request
		// always replays to its original ledger entry, including withdrawn ones.
		if existingID, exists := state.ApprovalRequestKeys[requestIndex]; exists {
			existing, found := state.Approvals[existingID]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "approval index is inconsistent", map[string]any{
					"approval_id": existingID,
				})
			}
			if existing.RequestFingerprint != fingerprint {
				return domain.NewError(domain.CodeIdempotencyConflict, "request key was reused with different approval content", map[string]any{
					"plan_id":     planID,
					"request_key": input.RequestKey,
				})
			}
			result = existing
			return nil
		}

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
		if plan.LatestRevisionID == "" {
			return domain.InvalidState("plan has no zone revision to approve", map[string]any{
				"plan_id": planID,
			})
		}
		revision, ok := state.Revisions[plan.LatestRevisionID]
		if !ok {
			return domain.NewError(domain.CodeStorageFailure, "plan references a missing revision", map[string]any{
				"revision_id": plan.LatestRevisionID,
			})
		}

		// A still-live decision by the same role on the exact revision/version
		// blocks duplicate approval. Withdrawn, expired or version-superseded
		// records never block, so reviewers can re-approve after changes.
		now := s.clock.Now().UTC()
		for _, approval := range state.Approvals {
			if approval.PlanID != planID ||
				approval.Role != input.Role ||
				approval.RevisionID != revision.ID ||
				approval.PlanVersion != plan.Version ||
				approval.Status != domain.ApprovalStatusActive {
				continue
			}
			if !approval.ExpiresAt.IsZero() && !now.Before(approval.ExpiresAt) {
				continue
			}
			return domain.NewError(domain.CodeConflict, "role already has an active approval for this plan version", map[string]any{
				"plan_id":     planID,
				"role":        input.Role,
				"approval_id": approval.ID,
			})
		}

		ttl := time.Duration(input.ExpiresInSeconds) * time.Second
		if ttl == 0 {
			ttl = domain.DefaultApprovalTTL
		}
		created := domain.PlanApproval{
			ID:                 s.ids.New("apr"),
			PlanID:             planID,
			RevisionID:         revision.ID,
			RevisionDigest:     domain.RevisionDigest(revision),
			PlanVersion:        plan.Version,
			Role:               input.Role,
			Approver:           input.Approver,
			Decision:           input.Decision,
			Status:             domain.ApprovalStatusActive,
			Note:               input.Note,
			RequestKey:         input.RequestKey,
			RequestFingerprint: fingerprint,
			CreatedAt:          now,
			ExpiresAt:          now.Add(ttl),
		}

		state.Approvals[created.ID] = created
		state.ApprovalRequestKeys[requestIndex] = created.ID
		store.AddAudit(state, s.ids, "approval", created.ID, "recorded", now, map[string]any{
			"plan_id":      planID,
			"revision_id":  revision.ID,
			"plan_version": plan.Version,
			"role":         created.Role,
			"decision":     created.Decision,
		})
		result = created
		return nil
	})
	return result, err
}

// Withdraw marks an approval withdrawn. Only the original approver may withdraw
// it. Withdrawal is permanent and audited; a withdrawn approval can never
// authorize activation.
func (s *Service) Withdraw(
	ctx context.Context,
	approvalID string,
	input domain.ApprovalWithdrawInput,
) (domain.PlanApproval, error) {
	input.Approver = strings.TrimSpace(input.Approver)
	input.RequestKey = strings.TrimSpace(input.RequestKey)
	if err := domain.ValidateApprovalWithdrawInput(input); err != nil {
		return domain.PlanApproval{}, err
	}
	requestIndex := "withdraw\x00" + approvalID + "\x00" + input.RequestKey
	var result domain.PlanApproval
	err := s.store.Write(ctx, func(state *domain.State) error {
		if existingID, exists := state.ApprovalRequestKeys[requestIndex]; exists {
			existing, found := state.Approvals[existingID]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "approval index is inconsistent", map[string]any{
					"approval_id": existingID,
				})
			}
			// A replayed withdrawal must carry the same approver identity.
			if existing.Approver != input.Approver {
				return domain.NewError(domain.CodeIdempotencyConflict, "request key was reused with different withdraw content", map[string]any{
					"approval_id": approvalID,
					"request_key": input.RequestKey,
				})
			}
			result = existing
			return nil
		}

		approval, ok := state.Approvals[approvalID]
		if !ok {
			return domain.NotFound("approval", approvalID)
		}
		if approval.Approver != input.Approver {
			return domain.NewError(domain.CodeConflict, "approval can be withdrawn only by its approver", map[string]any{
				"approval_id": approvalID,
			})
		}
		if approval.Status == domain.ApprovalStatusWithdrawn {
			result = approval
			return nil
		}
		now := s.clock.Now().UTC()
		withdrawnAt := now
		approval.Status = domain.ApprovalStatusWithdrawn
		approval.WithdrawnAt = &withdrawnAt
		state.Approvals[approvalID] = approval
		state.ApprovalRequestKeys[requestIndex] = approvalID
		store.AddAudit(state, s.ids, "approval", approvalID, "withdrawn", now, map[string]any{
			"plan_id": approval.PlanID,
			"role":    approval.Role,
		})
		result = approval
		return nil
	})
	return result, err
}

func (s *Service) Get(ctx context.Context, approvalID string) (domain.PlanApproval, error) {
	var result domain.PlanApproval
	err := s.store.Read(ctx, func(state *domain.State) error {
		approval, ok := state.Approvals[approvalID]
		if !ok {
			return domain.NotFound("approval", approvalID)
		}
		result = approval
		return nil
	})
	return result, err
}

// ListForPlan returns the approval ledger for a plan in stable chronological
// order with cursor pagination.
func (s *Service) ListForPlan(
	ctx context.Context,
	planID string,
	limit int,
	cursor string,
) (domain.ApprovalPage, error) {
	if limit < 1 || limit > 100 {
		return domain.ApprovalPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.ApprovalPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.ApprovalPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		if _, ok := state.Plans[planID]; !ok {
			return domain.NotFound("plan", planID)
		}
		items := make([]domain.PlanApproval, 0)
		for _, approval := range state.Approvals {
			if approval.PlanID == planID {
				items = append(items, approval)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].CreatedAt.Before(items[j].CreatedAt)
			}
			return items[i].ID < items[j].ID
		})
		if after != "" {
			filtered := make([]domain.PlanApproval, 0, len(items))
			for _, approval := range items {
				if approval.ID > after {
					filtered = append(filtered, approval)
				}
			}
			items = filtered
		}
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}
