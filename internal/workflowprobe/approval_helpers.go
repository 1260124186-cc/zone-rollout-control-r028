package workflowprobe

import (
	"fmt"
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
)

func createDraftPlan(
	harness *harness,
	zone string,
	requiresDualApproval bool,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(http.MethodPut, "/v1/plans", map[string]any{
		"zone":                   zone,
		"minimum_success_ratio":  1,
		"maximum_latency_ms":     80,
		"expected_nodes":         1,
		"requires_dual_approval": requiresDualApproval,
	}, &plan)
	return plan, err
}

func addRevision(harness *harness, planID string, serial uint32, zone string) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	err := harness.call(http.MethodPatch, "/v1/plans/"+planID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1." + zone,
		Entries: []domain.ZoneEntry{
			{Owner: zone, Type: domain.EntryA, Value: "192.0.2.10"},
		},
	}, &revision)
	return revision, err
}

func recordApproval(
	harness *harness,
	planID string,
	requestKey string,
	role domain.ApprovalRole,
	reviewer string,
	decision domain.ApprovalDecision,
) (domain.PlanApproval, error) {
	var approval domain.PlanApproval
	err := harness.call(http.MethodPost, "/v1/plans/"+planID+"/approvals", map[string]any{
		"request_key": requestKey,
		"role":        string(role),
		"reviewer_id": reviewer,
		"decision":    string(decision),
		"note":        "checked before release",
	}, &approval)
	return approval, err
}

func dualApprove(
	harness *harness,
	planID string,
	reviewerKey string,
	approverKey string,
) (domain.PlanApproval, domain.PlanApproval, error) {
	review, err := recordApproval(harness, planID, reviewerKey, domain.RoleReviewer, "alice.review", domain.DecisionApproved)
	if err != nil {
		return domain.PlanApproval{}, domain.PlanApproval{}, err
	}
	approval, err := recordApproval(harness, planID, approverKey, domain.RoleApprover, "bob.approve", domain.DecisionApproved)
	if err != nil {
		return domain.PlanApproval{}, domain.PlanApproval{}, err
	}
	return review, approval, nil
}

func getSummary(harness *harness, planID string) (domain.ApprovalSummary, error) {
	var summary domain.ApprovalSummary
	err := harness.call(http.MethodGet, "/v1/plans/"+planID+"/approvals", nil, &summary)
	return summary, err
}

func getApprovalView(harness *harness, approvalID string) (domain.ApprovalView, error) {
	var view domain.ApprovalView
	err := harness.call(http.MethodGet, "/v1/approvals/"+approvalID, nil, &view)
	return view, err
}

func withdrawApproval(harness *harness, approvalID string, reviewer string) (domain.PlanApproval, error) {
	var approval domain.PlanApproval
	err := harness.call(http.MethodPost, "/v1/approvals/"+approvalID+"/withdraw", map[string]any{
		"reviewer_id": reviewer,
	}, &approval)
	return approval, err
}

func activateExpecting(
	harness *harness,
	planID string,
	version uint64,
	target any,
) error {
	return harness.call(http.MethodPatch, "/v1/plans/"+planID+"/activate", map[string]any{
		"expected_version": version,
	}, target)
}

// activateConcurrently fires the same activation from several goroutines.
// Exactly one may succeed; the rest must fail with a stable conflict.
func activateConcurrently(harness *harness, planID string, version uint64, count int) (int, int, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, failures := 0, 0
	statuses := make(map[string]int)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var plan domain.RolloutPlan
			err := activateExpecting(harness, planID, version, &plan)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				if plan.State == domain.PlanActive {
					successes++
				}
				return
			}
			if failure, ok := err.(*apiFailure); ok {
				failures++
				statuses[failure.Code]++
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		return successes, failures, fmt.Errorf("expected exactly one successful concurrent activation, got %d (failure codes %v)", successes, statuses)
	}
	if failures != count-1 {
		return successes, failures, fmt.Errorf("expected %d failed concurrent activations, got %d (codes %v)", count-1, failures, statuses)
	}
	for code := range statuses {
		if code != string(domain.CodeInvalidState) && code != string(domain.CodeVersionConflict) {
			return successes, failures, fmt.Errorf("unexpected concurrent failure code %q", code)
		}
	}
	return successes, failures, nil
}
