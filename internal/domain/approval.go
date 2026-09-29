package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxReviewerIDRunes   = 180
	MaxApprovalNoteRunes = 500
)

// Stable reason codes reported when an approval cannot release activation.
const (
	ApprovalReasonActiveRejection  = "active_rejection"
	ApprovalReasonMissingRole      = "approval_missing_role"
	ApprovalReasonReviewerConflict = "approval_reviewers_must_distinct"
)

// Ineffective codes recorded on approval read views.
const (
	ApprovalIneffectiveWithdrawn       = "withdrawn"
	ApprovalIneffectiveExpired         = "expired"
	ApprovalIneffectiveVersionChanged  = "version_changed"
	ApprovalIneffectiveRevisionChanged = "revision_changed"
)

type ApprovalCreateInput struct {
	RequestKey string `json:"request_key"`
	Role       string `json:"role"`
	ReviewerID string `json:"reviewer_id"`
	Decision   string `json:"decision"`
	Note       string `json:"note"`
}

type ApprovalWithdrawInput struct {
	ReviewerID string `json:"reviewer_id"`
}

// RevisionDigest fingerprints the immutable revision content an approval is
// bound to. Revisions never change, so equal digests prove equal content.
func RevisionDigest(revision ZoneRevision) string {
	payload, _ := json.Marshal(struct {
		Serial    uint32
		Authority string
		Entries   []ZoneEntry
	}{
		Serial:    revision.Serial,
		Authority: revision.Authority,
		Entries:   revision.Entries,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ApprovalFingerprint covers exactly the client-supplied approval payload.
// Server-computed binding fields are never part of it, so they cannot be
// forged through request-body replay.
func ApprovalFingerprint(planID string, input ApprovalCreateInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%s|%s|%s|%s",
		planID,
		input.RequestKey,
		input.Role,
		input.ReviewerID,
		input.Decision,
		input.Note,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// NormalizeApprovalInput canonicalizes caller-controlled approval fields.
func NormalizeApprovalInput(input ApprovalCreateInput) ApprovalCreateInput {
	input.RequestKey = strings.TrimSpace(input.RequestKey)
	input.Role = strings.TrimSpace(strings.ToLower(input.Role))
	input.ReviewerID = canonicalName(input.ReviewerID)
	input.Decision = strings.TrimSpace(strings.ToLower(input.Decision))
	input.Note = strings.TrimSpace(input.Note)
	return input
}

// ValidateApprovalInput validates shape only. Whether a role is admitted by
// the active policy is checked by the approval service.
func ValidateApprovalInput(input ApprovalCreateInput) error {
	input = NormalizeApprovalInput(input)
	details := make(map[string]any)
	if err := validateRequestKey(input.RequestKey); err != nil {
		details["request_key"] = err.Error()
	}
	if input.Role == "" {
		details["role"] = "is required"
	}
	if err := validateCanonicalName(input.ReviewerID); err != nil {
		details["reviewer_id"] = err.Error()
	}
	if input.Decision != string(DecisionApproved) && input.Decision != string(DecisionRejected) {
		details["decision"] = "must be approved or rejected"
	}
	if count := utf8.RuneCountInString(input.Note); count > MaxApprovalNoteRunes {
		details["note"] = "must be at most 500 characters"
	}
	if len(details) > 0 {
		return Validation("approval input is invalid", details)
	}
	return nil
}

// ValidateApprovalWithdrawInput validates a withdrawal request.
func ValidateApprovalWithdrawInput(input ApprovalWithdrawInput) error {
	input.ReviewerID = canonicalName(input.ReviewerID)
	if err := validateCanonicalName(input.ReviewerID); err != nil {
		return Validation("approval withdrawal input is invalid", map[string]any{
			"reviewer_id": err.Error(),
		})
	}
	return nil
}

// ApprovalValidity explains whether one stored approval is still good.
type ApprovalValidity struct {
	Current   bool
	Effective bool
	Code      string
}

// EvaluateApproval evaluates one approval against the current plan, its
// current revision, and the current instant.
func EvaluateApproval(
	approval PlanApproval,
	plan RolloutPlan,
	revision *ZoneRevision,
	now time.Time,
) ApprovalValidity {
	result := ApprovalValidity{Current: true, Effective: true}
	invalidate := func(code string) ApprovalValidity {
		return ApprovalValidity{Current: false, Effective: false, Code: code}
	}
	if approval.Status != ApprovalActive {
		return invalidate(ApprovalIneffectiveWithdrawn)
	}
	if !approval.ExpiresAt.IsZero() && now.After(approval.ExpiresAt) {
		return invalidate(ApprovalIneffectiveExpired)
	}
	if approval.PlanVersion != plan.Version {
		return invalidate(ApprovalIneffectiveVersionChanged)
	}
	if approval.RevisionID != plan.LatestRevisionID {
		return invalidate(ApprovalIneffectiveRevisionChanged)
	}
	if revision == nil || revision.ID != approval.RevisionID {
		return invalidate(ApprovalIneffectiveRevisionChanged)
	}
	if RevisionDigest(*revision) != approval.RevisionFingerprint || revision.Serial != approval.RevisionSerial {
		return invalidate(ApprovalIneffectiveRevisionChanged)
	}
	return result
}

// BuildApprovalViews projects stored approvals with per-approval validity.
// Approvals are ordered oldest first for stable, auditable output.
func BuildApprovalViews(
	approvals []PlanApproval,
	plan RolloutPlan,
	revision *ZoneRevision,
	now time.Time,
) []ApprovalView {
	sorted := append([]PlanApproval(nil), approvals...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	views := make([]ApprovalView, 0, len(sorted))
	for _, approval := range sorted {
		validity := EvaluateApproval(approval, plan, revision, now)
		views = append(views, ApprovalView{
			PlanApproval:    approval,
			Current:         validity.Current,
			Effective:       validity.Effective,
			IneffectiveCode: validity.Code,
		})
	}
	return views
}

// EvaluateActivationApprovals decides whether the ledger proves a still-valid
// dual approval for every required role. Approvals must come from distinct
// reviewers, and an effective rejection blocks activation outright.
func EvaluateActivationApprovals(
	approvals []PlanApproval,
	plan RolloutPlan,
	revision *ZoneRevision,
	requiredRoles []ApprovalRole,
	now time.Time,
) ApprovalEvaluation {
	views := BuildApprovalViews(approvals, plan, revision, now)
	slots := make(map[ApprovalRole]PlanApproval)
	reviewers := make(map[string]struct{})
	reasons := make(map[string]struct{})
	for _, view := range views {
		if !view.Effective {
			continue
		}
		approval := view.PlanApproval
		roleAdmitted := false
		for _, required := range requiredRoles {
			if approval.Role == required {
				roleAdmitted = true
				break
			}
		}
		if !roleAdmitted {
			continue
		}
		if approval.Decision == DecisionRejected {
			reasons[ApprovalReasonActiveRejection] = struct{}{}
			continue
		}
		if _, taken := slots[approval.Role]; taken {
			continue
		}
		if _, seen := reviewers[approval.ReviewerID]; seen {
			// The same identity cannot cover two roles in a dual review.
			reasons[ApprovalReasonReviewerConflict] = struct{}{}
			continue
		}
		slots[approval.Role] = approval
		reviewers[approval.ReviewerID] = struct{}{}
	}
	missing := make([]ApprovalRole, 0)
	for _, role := range requiredRoles {
		if _, ok := slots[role]; !ok {
			missing = append(missing, role)
		}
	}
	if len(missing) > 0 {
		reasons[ApprovalReasonMissingRole] = struct{}{}
	}
	reasonList := make([]string, 0, len(reasons))
	for reason := range reasons {
		reasonList = append(reasonList, reason)
	}
	sort.Strings(reasonList)
	return ApprovalEvaluation{
		Ready:        len(reasonList) == 0,
		MissingRoles: missing,
		Reasons:      reasonList,
	}
}
