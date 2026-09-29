package httpapi

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) recordApproval(w http.ResponseWriter, r *http.Request) {
	var input domain.ApprovalCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.approvals.Record(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, result)
}

func (s *Server) withdrawApproval(w http.ResponseWriter, r *http.Request) {
	var input domain.ApprovalWithdrawInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.approvals.Withdraw(r.Context(), r.PathValue("approvalId"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) {
	result, err := s.approvals.Get(r.Context(), r.PathValue("approvalId"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) listPlanApprovals(w http.ResponseWriter, r *http.Request) {
	result, err := s.approvals.SummaryForPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}
