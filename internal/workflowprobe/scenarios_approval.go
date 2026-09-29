package workflowprobe

import (
	"net/http"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/plan"
)

// approval-policy-disabled proves that with the policy not enabled, plans
// activate directly (as before) and the ledger endpoints stay readable; a
// plan that explicitly opts in is still gated.
func runApprovalPolicyDisabled() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	planRecord, err := createDraftPlan(harness, "legacy.example", false)
	if err != nil {
		return err
	}
	if planRecord.RequiresDualApproval {
		return ensure(false, "historical-style plan was created with dual approval unexpectedly")
	}
	revision, err := addRevision(harness, planRecord.ID, 2026092501, "legacy.example")
	if err != nil {
		return err
	}
	// No approvals recorded: activation still succeeds without policy.
	if err := activateExpecting(harness, planRecord.ID, planRecord.Version+1, &planRecord); err != nil {
		return err
	}
	if planRecord.State != domain.PlanActive {
		return ensure(false, "legacy plan did not activate without approvals")
	}
	_ = revision

	// The independent ledger remains readable and reports it is not gating.
	summary, err := getSummary(harness, planRecord.ID)
	if err != nil {
		return err
	}
	if summary.Required || summary.Ready {
		return ensure(false, "ledger summary must report required=false for legacy plan")
	}

	// An explicit opt-in plan is gated even when the policy defaults off.
	optIn, err := createDraftPlan(harness, "optin.example", true)
	if err != nil {
		return err
	}
	if !optIn.RequiresDualApproval {
		return ensure(false, "explicit opt-in plan lost its dual-approval flag")
	}
	if _, err := addRevision(harness, optIn.ID, 2026092601, "optin.example"); err != nil {
		return err
	}
	if err := activateExpecting(harness, optIn.ID, optIn.Version+1, nil); err == nil {
		return ensure(false, "opt-in plan activated without dual approval")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	return nil
}

// runDualApprovalGate covers the full mandatory policy boundary: missing
// approvals, dual signatures, forged binding fields, version drift,
// withdrawal, expiry, rejection, duplicate role decisions, and same-reviewer
// role conflicts.
func runDualApprovalGate() error {
	ttl := time.Hour
	policy := plan.DualApprovalPolicy{
		DefaultRequired: true,
		RequiredRoles:   []domain.ApprovalRole{domain.RoleReviewer, domain.RoleApprover},
		TTL:             ttl,
	}
	harness, err := newHarnessWithPolicy(policy)
	if err != nil {
		return err
	}
	defer harness.close()

	draft, err := createDraftPlan(harness, "risky.example", true)
	if err != nil {
		return err
	}
	revision, err := addRevision(harness, draft.ID, 2026092501, "risky.example")
	if err != nil {
		return err
	}
	draft.Version++

	// No approvals yet: activation is refused and the summary says why.
	if err := activateExpecting(harness, draft.ID, draft.Version, nil); err == nil {
		return ensure(false, "plan activated with no approvals")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	summary, err := getSummary(harness, draft.ID)
	if err != nil {
		return err
	}
	if summary.Ready || !summary.Required {
		return ensure(false, "summary must report required and not ready before dual approval")
	}
	if len(summary.MissingRoles) != 2 {
		return ensure(false, "summary must list both missing roles")
	}

	// One role is not enough.
	review, err := recordApproval(harness, draft.ID, "key-review-001", domain.RoleReviewer, "alice.review", domain.DecisionApproved)
	if err != nil {
		return err
	}
	if review.PlanVersion != draft.Version || review.RevisionID != revision.ID {
		return ensure(false, "approval was not bound to the current plan version and revision")
	}
	if review.RevisionSerial != revision.Serial || review.RevisionFingerprint == "" {
		return ensure(false, "approval is missing immutable revision binding")
	}
	if err := activateExpecting(harness, draft.ID, draft.Version, nil); err == nil {
		return ensure(false, "plan activated with a single approval")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}

	// Forged binding fields in the approval body cannot seed the ledger:
	// strict decoding rejects any field the entry point does not define.
	if err := harness.call(http.MethodPost, "/v1/plans/"+draft.ID+"/approvals", map[string]any{
		"request_key":          "key-forge-001",
		"role":                 "approver",
		"reviewer_id":          "mallory",
		"decision":             "approved",
		"plan_version":         999,
		"revision_id":          "rev_does_not_exist",
		"revision_serial":      42,
		"revision_fingerprint": "tampered",
		"status":               "active",
		"expires_at":           "2099-01-01T00:00:00Z",
	}, nil); err == nil {
		return ensure(false, "forged binding fields in the approval body were accepted")
	} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// Same reviewer cannot cover both roles.
	if _, err := recordApproval(harness, draft.ID, "key-review-again", domain.RoleApprover, "alice.review", domain.DecisionApproved); err == nil {
		return ensure(false, "same reviewer occupied two roles")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalConflict)); err != nil {
		return err
	}

	// Duplicate role decision is rejected; identical replay is idempotent.
	if _, err := recordApproval(harness, draft.ID, "key-review-dup", domain.RoleReviewer, "carol.other", domain.DecisionApproved); err == nil {
		return ensure(false, "duplicate reviewer decision was accepted")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalConflict)); err != nil {
		return err
	}
	replay, err := recordApproval(harness, draft.ID, "key-review-001", domain.RoleReviewer, "alice.review", domain.DecisionApproved)
	if err != nil {
		return err
	}
	if replay.ID != review.ID {
		return ensure(false, "identical approval replay did not return the original record")
	}
	// Same request key with different payload conflicts.
	if _, err := recordApproval(harness, draft.ID, "key-review-001", domain.RoleApprover, "bob.approve", domain.DecisionApproved); err == nil {
		return ensure(false, "request key reuse with different payload was accepted")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeIdempotencyConflict)); err != nil {
		return err
	}

	// Second role completes the dual approval; activation now succeeds.
	approval, err := recordApproval(harness, draft.ID, "key-approve-001", domain.RoleApprover, "bob.approve", domain.DecisionApproved)
	if err != nil {
		return err
	}
	summary, err = getSummary(harness, draft.ID)
	if err != nil {
		return err
	}
	if !summary.Ready {
		return ensure(false, "summary not ready with two distinct role approvals")
	}
	if err := activateExpecting(harness, draft.ID, draft.Version, &draft); err != nil {
		return err
	}
	if draft.State != domain.PlanActive {
		return ensure(false, "dual-approved plan did not activate")
	}

	// Approvals cannot be recorded against or withdrawn from a non-draft plan.
	if _, err := recordApproval(harness, draft.ID, "key-after-active", domain.RoleReviewer, "dan.late", domain.DecisionApproved); err == nil {
		return ensure(false, "approval recorded against an active plan")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	if _, err := withdrawApproval(harness, approval.ID, "bob.approve"); err == nil {
		return ensure(false, "approval withdrawn after activation")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Withdrawal is reviewer-only; already-withdrawn cannot be withdrawn again.
	second, err := newDraftWithRevision(harness, "second.example", 2026092502)
	if err != nil {
		return err
	}
	r2, a2, err := dualApprove(harness, second.ID, "key-r2-review", "key-r2-approve")
	if err != nil {
		return err
	}
	if _, err := withdrawApproval(harness, a2.ID, "not.bob"); err == nil {
		return ensure(false, "another reviewer withdrew someone else's approval")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalNotWithdrawn)); err != nil {
		return err
	}
	withdrawn, err := withdrawApproval(harness, r2.ID, "alice.review")
	if err != nil {
		return err
	}
	if withdrawn.Status != domain.ApprovalWithdrawn || withdrawn.WithdrawnAt == nil {
		return ensure(false, "withdrawal did not produce a withdrawn record")
	}
	if _, err := withdrawApproval(harness, r2.ID, "alice.review"); err == nil {
		return ensure(false, "withdrawn approval was withdrawn again")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalNotWithdrawn)); err != nil {
		return err
	}
	if err := activateExpecting(harness, second.ID, second.Version, nil); err == nil {
		return ensure(false, "plan activated after one approval was withdrawn")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	// Read view marks the withdrawn record ineffective, so stale evidence is visible.
	view, err := getApprovalView(harness, r2.ID)
	if err != nil {
		return err
	}
	if view.Effective || view.IneffectiveCode != domain.ApprovalIneffectiveWithdrawn {
		return ensure(false, "withdrawn approval not flagged ineffective")
	}

	// Re-approving with a new request key re-enables activation.
	r3, err := recordApproval(harness, second.ID, "key-r2-review-new", domain.RoleReviewer, "alice.review", domain.DecisionApproved)
	if err != nil {
		return err
	}
	if r3.ID == r2.ID {
		return ensure(false, "re-approval reused the withdrawn record")
	}
	if err := activateExpecting(harness, second.ID, second.Version, &second); err != nil {
		return err
	}

	// Version drift: an approval bound to an older revision cannot release a
	// newer one ("approvals then an extra revision").
	third, err := newDraftWithRevision(harness, "third.example", 2026092503)
	if err != nil {
		return err
	}
	if _, _, err := dualApprove(harness, third.ID, "key-r3-review", "key-r3-approve"); err != nil {
		return err
	}
	if _, err := addRevision(harness, third.ID, 2026092504, "third.example"); err != nil {
		return err
	}
	third.Version++
	// Old expected_version is first caught by optimistic concurrency.
	if err := activateExpecting(harness, third.ID, third.Version-1, nil); err == nil {
		return ensure(false, "stale version activation was accepted")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
		return err
	}
	// Even with the current version, the stale approvals no longer release.
	if err := activateExpecting(harness, third.ID, third.Version, nil); err == nil {
		return ensure(false, "stale approvals released activation for a newer revision")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	driftSummary, err := getSummary(harness, third.ID)
	if err != nil {
		return err
	}
	for _, entry := range driftSummary.Approvals {
		if entry.Effective {
			return ensure(false, "approval bound to an older revision is still effective")
		}
		if entry.IneffectiveCode != domain.ApprovalIneffectiveVersionChanged &&
			entry.IneffectiveCode != domain.ApprovalIneffectiveRevisionChanged {
			return ensure(false, "version-drift approval has an unexpected ineffective code")
		}
	}

	// Expiry: approvals past their TTL cannot release activation.
	fourth, err := newDraftWithRevision(harness, "fourth.example", 2026092505)
	if err != nil {
		return err
	}
	if _, _, err := dualApprove(harness, fourth.ID, "key-r4-review", "key-r4-approve"); err != nil {
		return err
	}
	harness.clock.Advance(ttl + time.Minute)
	if err := activateExpecting(harness, fourth.ID, fourth.Version, nil); err == nil {
		return ensure(false, "expired approvals released activation")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	expiredView, err := getSummary(harness, fourth.ID)
	if err != nil {
		return err
	}
	for _, entry := range expiredView.Approvals {
		if entry.IneffectiveCode != domain.ApprovalIneffectiveExpired {
			return ensure(false, "expired approval not marked expired")
		}
	}

	// An active rejection blocks even when both role slots are otherwise present.
	fifth, err := newDraftWithRevision(harness, "fifth.example", 2026092506)
	if err != nil {
		return err
	}
	if _, err := recordApproval(harness, fifth.ID, "key-r5-review", domain.RoleReviewer, "alice.review", domain.DecisionApproved); err != nil {
		return err
	}
	if _, err := recordApproval(harness, fifth.ID, "key-r5-reject", domain.RoleApprover, "bob.approve", domain.DecisionRejected); err != nil {
		return err
	}
	if err := activateExpecting(harness, fifth.ID, fifth.Version, nil); err == nil {
		return ensure(false, "plan activated despite an active rejection")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeApprovalRequired)); err != nil {
		return err
	}
	rejectSummary, err := getSummary(harness, fifth.ID)
	if err != nil {
		return err
	}
	if len(rejectSummary.ActiveRejections) != 1 {
		return ensure(false, "summary did not surface the active rejection")
	}

	// Approvals require a revision first.
	empty, err := createDraftPlan(harness, "empty.example", true)
	if err != nil {
		return err
	}
	if _, err := recordApproval(harness, empty.ID, "key-empty-review", domain.RoleReviewer, "alice.review", domain.DecisionApproved); err == nil {
		return ensure(false, "approval accepted on a revisionless draft")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Unknown roles and bad input are rejected at the boundary.
	if _, err := recordApproval(harness, draft.ID, "key-bad-role", domain.ApprovalRole("manager"), "eve.manager", domain.DecisionApproved); err == nil {
		return ensure(false, "unknown approval role accepted")
	} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	return nil
}

func newDraftWithRevision(harness *harness, zone string, serial uint32) (domain.RolloutPlan, error) {
	planRecord, err := createDraftPlan(harness, zone, true)
	if err != nil {
		return domain.RolloutPlan{}, err
	}
	if _, err := addRevision(harness, planRecord.ID, serial, zone); err != nil {
		return domain.RolloutPlan{}, err
	}
	planRecord.Version++
	return planRecord, nil
}

// runApprovalConcurrentActivation proves that even with a valid dual
// approval, concurrent activation requests have one winner; the gate does not
// allow two transitions through.
func runApprovalConcurrentActivation() error {
	policy := plan.DualApprovalPolicy{
		DefaultRequired: true,
		RequiredRoles:   []domain.ApprovalRole{domain.RoleReviewer, domain.RoleApprover},
		TTL:             time.Hour,
	}
	harness, err := newHarnessWithPolicy(policy)
	if err != nil {
		return err
	}
	defer harness.close()

	draft, err := newDraftWithRevision(harness, "concurrent.example", 2026092507)
	if err != nil {
		return err
	}
	if _, _, err := dualApprove(harness, draft.ID, "key-rc-review", "key-rc-approve"); err != nil {
		return err
	}
	successes, failures, err := activateConcurrently(harness, draft.ID, draft.Version, 8)
	if err != nil {
		return err
	}
	if successes != 1 || failures != 7 {
		return ensure(false, "concurrent activation did not have a single deterministic winner")
	}

	// A later activation is now an illegal transition regardless of approvals.
	if err := activateExpecting(harness, draft.ID, draft.Version+1, nil); err == nil {
		return ensure(false, "already-active plan activated twice")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	return nil
}
