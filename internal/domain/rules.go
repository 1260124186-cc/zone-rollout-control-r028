package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

func ActivatePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanDraft {
		return InvalidState("plan can be activated only from draft", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	if plan.LatestRevisionID == "" {
		return InvalidState("plan needs a zone revision before activation", map[string]any{
			"plan_id": plan.ID,
		})
	}
	plan.State = PlanActive
	plan.ActivatedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func CompletePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanActive {
		return InvalidState("plan can be completed only from active", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	plan.State = PlanCompleted
	plan.CompletedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func LeaseFingerprint(planID string, input LeaseInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%s|%d|%d",
		planID,
		input.RequestKey,
		input.Region,
		input.LeaseSeconds,
		input.MaxNodes,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func ProbeFingerprint(leaseID string, input ProbeInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%t|%d|%d|%s",
		leaseID,
		input.RequestKey,
		input.Success,
		input.ObservedSerial,
		input.LatencyMS,
		input.ObservedAt.UTC().Format(time.RFC3339Nano),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func BuildAssessment(
	id string,
	plan RolloutPlan,
	revision ZoneRevision,
	probes []Probe,
	now time.Time,
) RolloutAssessment {
	reasons := make(map[string]struct{})
	if len(probes) == 0 {
		reasons["no_probes"] = struct{}{}
	}
	probeCount := len(probes)
	successCount := 0
	maximumLatency := 0
	nodes := make(map[string]struct{})
	for _, probe := range probes {
		nodes[probe.NodeID] = struct{}{}
		if probe.LatencyMS > maximumLatency {
			maximumLatency = probe.LatencyMS
		}
		if probe.Success {
			successCount++
			if probe.ObservedSerial != revision.Serial {
				reasons["serial_mismatch"] = struct{}{}
			}
		}
	}
	if len(nodes) < plan.ExpectedNodes {
		reasons["node_not_observed"] = struct{}{}
	}
	successRatio := 0.0
	if probeCount > 0 {
		successRatio = round(float64(successCount)/float64(probeCount), 6)
	}
	if successRatio < plan.MinimumSuccessRatio {
		reasons["success_ratio_below_minimum"] = struct{}{}
	}
	if maximumLatency > plan.MaximumLatencyMS {
		reasons["latency_limit_exceeded"] = struct{}{}
	}
	reasonList := make([]string, 0, len(reasons))
	for reason := range reasons {
		reasonList = append(reasonList, reason)
	}
	sort.Strings(reasonList)
	verdict := VerdictConsistent
	if len(reasonList) > 0 {
		verdict = VerdictReviewRequired
	}
	return RolloutAssessment{
		ID:               id,
		PlanID:           plan.ID,
		RevisionID:       revision.ID,
		Verdict:          verdict,
		Reasons:          reasonList,
		ProbeCount:       probeCount,
		SuccessCount:     successCount,
		SuccessRatio:     successRatio,
		MaximumLatencyMS: maximumLatency,
		CreatedAt:        now.UTC(),
	}
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

// DefaultApprovalTTL is applied when the approver does not state a TTL.
const DefaultApprovalTTL = 24 * time.Hour

// RevisionDigest is the immutable fingerprint of a zone revision's content.
// Approvals bind to it, so a revision can never be silently swapped underneath
// an existing approval.
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

// ApprovalRequestFingerprint covers only the client-supplied approval payload.
// Server-side bindings (revision id, immutable digest, plan version) are stored
// as explicit columns, never taken from the body, so a replay cannot forge
// what was approved. The TTL is normalized to its effective value before
// hashing so omitting it matches a replay that also omits it.
func ApprovalRequestFingerprint(planID string, input ApprovalCreateInput) string {
	ttl := input.ExpiresInSeconds
	if ttl == 0 {
		ttl = int(DefaultApprovalTTL / time.Second)
	}
	payload := fmt.Sprintf(
		"%s|%s|%s|%s|%s|%s|%d",
		planID,
		input.RequestKey,
		input.Role,
		input.Approver,
		input.Decision,
		input.Note,
		ttl,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// EffectiveApprovalPolicy treats historical snapshots without a stored policy
// as ApprovalPolicyNone so legacy plans keep activating without approvals.
func EffectiveApprovalPolicy(plan RolloutPlan) ApprovalPolicy {
	if plan.ApprovalPolicy == ApprovalPolicyDualRole {
		return ApprovalPolicyDualRole
	}
	return ApprovalPolicyNone
}

// ApprovalMissingReason explains why the current ledger does not authorize
// activation. Stable codes are returned in the error details.
type ApprovalMissingReason string

const (
	ApprovalMissingRole  ApprovalMissingReason = "missing_role"
	ApprovalRejected     ApprovalMissingReason = "rejected"
	ApprovalSameApprover ApprovalMissingReason = "same_approver"
	ApprovalConflicting  ApprovalMissingReason = "conflicting_records"
)

// ApprovalGate evaluates the independent approval ledger for a plan that is
// about to activate. It accepts only still-valid dual-role "approved" records
// bound to the exact current revision (by identity and immutable digest) and
// current plan version. Withdrawn, expired, stale-revision or stale-version
// records never authorize activation.
func ApprovalGate(
	plan RolloutPlan,
	currentRevision ZoneRevision,
	approvals []PlanApproval,
	now time.Time,
) (map[ApprovalRole]PlanApproval, error) {
	matched := make(map[ApprovalRole][]PlanApproval)
	now = now.UTC()
	for _, approval := range approvals {
		if approval.PlanID != plan.ID {
			continue
		}
		if approval.Status != ApprovalStatusActive {
			continue
		}
		if !approval.ExpiresAt.IsZero() && !now.Before(approval.ExpiresAt) {
			continue
		}
		if approval.RevisionID != currentRevision.ID ||
			approval.RevisionDigest != RevisionDigest(currentRevision) ||
			approval.PlanVersion != plan.Version {
			continue
		}
		if approval.Role != ApprovalRoleRiskReviewer && approval.Role != ApprovalRoleZoneOwner {
			continue
		}
		matched[approval.Role] = append(matched[approval.Role], approval)
	}

	chosen := make(map[ApprovalRole]PlanApproval)
	details := make(map[string]any)
	for _, role := range []ApprovalRole{ApprovalRoleRiskReviewer, ApprovalRoleZoneOwner} {
		records := matched[role]
		if len(records) == 0 {
			details[string(role)] = string(ApprovalMissingRole)
			continue
		}
		sort.Slice(records, func(i, j int) bool {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		})
		active := records[len(records)-1]
		if active.Decision != ApprovalDecisionApproved {
			details[string(role)] = string(ApprovalRejected)
			continue
		}
		for _, record := range records[:len(records)-1] {
			if record.Decision == ApprovalDecisionApproved {
				details[string(role)] = string(ApprovalConflicting)
				break
			}
		}
		chosen[role] = active
	}
	if len(chosen) == 2 {
		risk := chosen[ApprovalRoleRiskReviewer]
		zone := chosen[ApprovalRoleZoneOwner]
		if risk.Approver == zone.Approver {
			return nil, InvalidState("dual-role approval requires two distinct approvers", map[string]any{
				"plan_id": plan.ID,
				"reason":  string(ApprovalSameApprover),
			})
		}
		return chosen, nil
	}
	if _, exists := details["reason"]; !exists && len(details) > 0 {
		details["plan_id"] = plan.ID
	}
	return nil, InvalidState("plan requires two valid role approvals before activation", details)
}

func timePointer(value time.Time) *time.Time {
	return &value
}
