package workflowprobe

import (
	"net/http"
	"sync"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// runDualRoleApprovalGate exercises the independent approval ledger: legacy
// compatibility when the policy is off, server-side binding of approvals to
// immutable revision/version, forgery rejection, idempotency, duplicate
// approvals, same-approver and rejected decisions, withdrawal, expiry,
// post-approval revisions and concurrent activation.
func runDualRoleApprovalGate() error {
	if err := probeLegacyPlansActivateWithoutApprovals(); err != nil {
		return err
	}
	if err := probeDualRoleFlow(); err != nil {
		return err
	}
	if err := probeVersionAndTimeInvalidation(); err != nil {
		return err
	}
	return nil
}

func createDraftPlanWithPolicy(
	harness *harness,
	zone string,
	policy domain.ApprovalPolicy,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(http.MethodPut, "/v1/plans", map[string]any{
		"zone":                  zone,
		"minimum_success_ratio": 1,
		"maximum_latency_ms":    80,
		"expected_nodes":        1,
		"approval_policy":       string(policy),
	}, &plan)
	return plan, err
}

func addPlanRevision(harness *harness, planID string, serial uint32) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	err := harness.call(http.MethodPatch, "/v1/plans/"+planID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1.edge.example",
		Entries: []domain.ZoneEntry{
			{Owner: "edge.example", Type: domain.EntryA, Value: "192.0.2.10"},
		},
	}, &revision)
	return revision, err
}

func activatePlanVersion(harness *harness, planID string, version uint64, body any) error {
	if body == nil {
		body = map[string]any{"expected_version": version}
	}
	var plan domain.RolloutPlan
	return harness.call(http.MethodPatch, "/v1/plans/"+planID+"/activate", body, &plan)
}

func recordApproval(
	harness *harness,
	planID string,
	requestKey string,
	role domain.ApprovalRole,
	approver string,
	decision domain.ApprovalDecision,
	ttlSeconds int,
) (domain.PlanApproval, error) {
	var approval domain.PlanApproval
	err := harness.call(http.MethodPatch, "/v1/plans/"+planID+"/approvals", domain.ApprovalCreateInput{
		RequestKey:       requestKey,
		Role:             role,
		Approver:         approver,
		Decision:         decision,
		ExpiresInSeconds: ttlSeconds,
	}, &approval)
	return approval, err
}

func probeLegacyPlansActivateWithoutApprovals() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	// Explicit "none" policy behaves like historical plans.
	plan, err := createDraftPlanWithPolicy(harness, "none.edge.example", domain.ApprovalPolicyNone)
	if err != nil {
		return err
	}
	if plan.ApprovalPolicy != "" && plan.ApprovalPolicy != domain.ApprovalPolicyNone {
		return ensure(false, "none policy was not stored")
	}
	if _, err := addPlanRevision(harness, plan.ID, 2026092501); err != nil {
		return err
	}
	if err := activatePlanVersion(harness, plan.ID, plan.Version+1, nil); err != nil {
		return err
	}

	// Plans created with no policy field at all stay on the legacy path.
	plan, _, err = createActivePlan(harness, "legacy.edge.example", 2026092601, 1, 1, 80)
	if err != nil {
		return err
	}
	if plan.State != domain.PlanActive {
		return ensure(false, "legacy plan did not activate without approvals")
	}
	return nil
}

func probeDualRoleFlow() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	plan, err := createDraftPlanWithPolicy(harness, "dual.edge.example", domain.ApprovalPolicyDualRole)
	if err != nil {
		return err
	}
	if plan.ApprovalPolicy != domain.ApprovalPolicyDualRole {
		return ensure(false, "dual_role policy was not stored")
	}
	revision, err := addPlanRevision(harness, plan.ID, 2026092701)
	if err != nil {
		return err
	}
	activationVersion := plan.Version + 1

	// No approvals yet: activation is refused.
	err = activatePlanVersion(harness, plan.ID, activationVersion, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Approval content cannot be smuggled through the activation request body.
	err = activatePlanVersion(harness, plan.ID, activationVersion, map[string]any{
		"expected_version": activationVersion,
		"approvals": []map[string]any{
			{"role": "risk_reviewer", "approver": "risk-a"},
			{"role": "zone_owner", "approver": "owner-a"},
		},
	})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// Unknown policy values are rejected at plan creation.
	if _, err := createDraftPlanWithPolicy(harness, "bogus.edge.example", domain.ApprovalPolicy("bogus")); err != nil {
		if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
			return err
		}
	} else {
		return ensure(false, "bogus approval policy was accepted")
	}

	// Recorded approval is bound to server-side revision/version/digest.
	risk, err := recordApproval(harness, plan.ID, "risk-key-0001",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 0)
	if err != nil {
		return err
	}
	if risk.RevisionID != revision.ID || risk.RevisionDigest == "" ||
		risk.PlanVersion != activationVersion || risk.Status != domain.ApprovalStatusActive {
		return ensure(false, "approval was not bound to the immutable revision and plan version")
	}
	if risk.ExpiresAt.Sub(risk.CreatedAt) != 24*time.Hour {
		return ensure(false, "default approval TTL was not applied")
	}

	// One role is not enough.
	err = activatePlanVersion(harness, plan.ID, activationVersion, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Same approver in both roles does not constitute dual-person review.
	zoneSame, err := recordApproval(harness, plan.ID, "zone-key-0001",
		domain.ApprovalRoleZoneOwner, "risk-a", domain.ApprovalDecisionApproved, 0)
	if err != nil {
		return err
	}
	err = activatePlanVersion(harness, plan.ID, activationVersion, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Idempotent replay returns the same ledger entry.
	replay, err := recordApproval(harness, plan.ID, "risk-key-0001",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 0)
	if err != nil {
		return err
	}
	if replay.ID != risk.ID {
		return ensure(false, "approval replay created a second entry")
	}

	// Same request key with different content conflicts.
	_, err = recordApproval(harness, plan.ID, "risk-key-0001",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionRejected, 0)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeIdempotencyConflict)); err != nil {
		return err
	}

	// A second active decision for the same role/version duplicates review.
	_, err = recordApproval(harness, plan.ID, "risk-key-0002",
		domain.ApprovalRoleRiskReviewer, "risk-b", domain.ApprovalDecisionApproved, 0)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeConflict)); err != nil {
		return err
	}

	// Withdrawal by someone else is refused.
	err = harness.call(http.MethodPatch, "/v1/approvals/"+zoneSame.ID+"/withdraw", domain.ApprovalWithdrawInput{
		RequestKey: "withdraw-key-0001",
		Approver:   "intruder",
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeConflict)); err != nil {
		return err
	}

	// Withdrawal by the original approver succeeds and invalidates the grant.
	var withdrawn domain.PlanApproval
	if err := harness.call(http.MethodPatch, "/v1/approvals/"+zoneSame.ID+"/withdraw", domain.ApprovalWithdrawInput{
		RequestKey: "withdraw-key-0002",
		Approver:   "risk-a",
	}, &withdrawn); err != nil {
		return err
	}
	if withdrawn.Status != domain.ApprovalStatusWithdrawn || withdrawn.WithdrawnAt == nil {
		return ensure(false, "approval was not marked withdrawn")
	}
	if err := activatePlanVersion(harness, plan.ID, activationVersion, nil); err != nil {
		if f, ok := err.(*apiFailure); !ok || f.Code != string(domain.CodeInvalidState) {
			return err
		}
	} else {
		return ensure(false, "activation passed after withdrawal")
	}

	// Withdrawing again is idempotent for the same request key.
	if err := harness.call(http.MethodPatch, "/v1/approvals/"+zoneSame.ID+"/withdraw", domain.ApprovalWithdrawInput{
		RequestKey: "withdraw-key-0002",
		Approver:   "risk-a",
	}, nil); err != nil {
		return err
	}

	// A distinct second person can now hold the zone-owner grant.
	zone, err := recordApproval(harness, plan.ID, "zone-key-0002",
		domain.ApprovalRoleZoneOwner, "owner-a", domain.ApprovalDecisionApproved, 0)
	if err != nil {
		return err
	}

	// Ledger reads expose the independent book of record.
	var page domain.ApprovalPage
	if err := harness.call(http.MethodGet, "/v1/plans/"+plan.ID+"/approvals?limit=10", nil, &page); err != nil {
		return err
	}
	if len(page.Items) != 3 {
		return ensure(false, "approval ledger did not list three entries")
	}
	var fetched domain.PlanApproval
	if err := harness.call(http.MethodGet, "/v1/approvals/"+zone.ID, nil, &fetched); err != nil {
		return err
	}
	if fetched.ID != zone.ID {
		return ensure(false, "approval lookup returned the wrong entry")
	}

	// Concurrent activation with the same expected version has exactly one
	// winner; the loser sees the optimistic version conflict.
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var activated domain.RolloutPlan
			results <- harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/activate", map[string]any{
				"expected_version": activationVersion,
			}, &activated)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	okCount, conflictCount := 0, 0
	for activationErr := range results {
		switch {
		case activationErr == nil:
			okCount++
		default:
			if f, isFailure := activationErr.(*apiFailure); isFailure &&
				f.Status == http.StatusConflict && f.Code == string(domain.CodeVersionConflict) {
				conflictCount++
				continue
			}
			return activationErr
		}
	}
	if okCount != 1 || conflictCount != 1 {
		return ensure(false, "concurrent activation did not resolve to one success and one version conflict")
	}

	// No further approvals can be appended once the plan is active.
	_, err = recordApproval(harness, plan.ID, "late-key-0001",
		domain.ApprovalRoleRiskReviewer, "risk-c", domain.ApprovalDecisionApproved, 0)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	return nil
}

func probeVersionAndTimeInvalidation() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	// Expired approvals no longer authorize activation.
	expiringPlan, err := createDraftPlanWithPolicy(harness, "ttl.edge.example", domain.ApprovalPolicyDualRole)
	if err != nil {
		return err
	}
	if _, err := addPlanRevision(harness, expiringPlan.ID, 2026092801); err != nil {
		return err
	}
	if _, err := recordApproval(harness, expiringPlan.ID, "ttl-risk-0001",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 600); err != nil {
		return err
	}
	if _, err := recordApproval(harness, expiringPlan.ID, "ttl-zone-0001",
		domain.ApprovalRoleZoneOwner, "owner-a", domain.ApprovalDecisionApproved, 600); err != nil {
		return err
	}
	if err := activatePlanVersion(harness, expiringPlan.ID, expiringPlan.Version+1, nil); err != nil {
		return err
	}

	// A new plan proves expiry invalidation and a rejected decision.
	stalePlan, err := createDraftPlanWithPolicy(harness, "stale.edge.example", domain.ApprovalPolicyDualRole)
	if err != nil {
		return err
	}
	if _, err := addPlanRevision(harness, stalePlan.ID, 2026092901); err != nil {
		return err
	}
	riskApproval, err := recordApproval(harness, stalePlan.ID, "stale-risk-0001",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 600)
	if err != nil {
		return err
	}
	rejected, err := recordApproval(harness, stalePlan.ID, "stale-zone-0001",
		domain.ApprovalRoleZoneOwner, "owner-a", domain.ApprovalDecisionRejected, 600)
	if err != nil {
		return err
	}
	if err := activatePlanVersion(harness, stalePlan.ID, stalePlan.Version+1, nil); err != nil {
		if f, ok := err.(*apiFailure); !ok || f.Code != string(domain.CodeInvalidState) {
			return err
		}
	} else {
		return ensure(false, "activation passed with a rejected approval")
	}

	// Time moves past the approval TTL.
	harness.clock.Advance(601 * 1e9)
	if err := activatePlanVersion(harness, stalePlan.ID, stalePlan.Version+1, nil); err != nil {
		if f, ok := err.(*apiFailure); !ok || f.Code != string(domain.CodeInvalidState) {
			return err
		}
	} else {
		return ensure(false, "activation passed after approvals expired")
	}

	// The expired role grants can be replaced because old live records no
	// longer count as duplicates.
	newRisk, err := recordApproval(harness, stalePlan.ID, "stale-risk-0002",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 3600)
	if err != nil {
		return err
	}
	_ = riskApproval
	_ = rejected
	if _, err := recordApproval(harness, stalePlan.ID, "stale-zone-0002",
		domain.ApprovalRoleZoneOwner, "owner-b", domain.ApprovalDecisionApproved, 3600); err != nil {
		return err
	}
	_ = newRisk

	// Adding a revision after approval bumps the plan version and strands the
	// grants: activation at the new version must be refused.
	if _, err := addPlanRevision(harness, stalePlan.ID, 2026092902); err != nil {
		return err
	}
	var stale domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+stalePlan.ID, nil, &stale); err != nil {
		return err
	}
	err = activatePlanVersion(harness, stale.ID, stale.Version, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Fresh dual-role approvals on the new version unlock activation.
	if _, err := recordApproval(harness, stale.ID, "stale-risk-0003",
		domain.ApprovalRoleRiskReviewer, "risk-a", domain.ApprovalDecisionApproved, 3600); err != nil {
		return err
	}
	if _, err := recordApproval(harness, stale.ID, "stale-zone-0003",
		domain.ApprovalRoleZoneOwner, "owner-b", domain.ApprovalDecisionApproved, 3600); err != nil {
		return err
	}
	if err := activatePlanVersion(harness, stale.ID, stale.Version, nil); err != nil {
		return err
	}
	return nil
}
