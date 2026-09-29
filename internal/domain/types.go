package domain

import "time"

type PlanState string

const (
	PlanDraft     PlanState = "draft"
	PlanActive    PlanState = "active"
	PlanCompleted PlanState = "completed"
)

type NodeState string

const (
	NodeReady       NodeState = "ready"
	NodeLeased      NodeState = "leased"
	NodeQuarantined NodeState = "quarantined"
)

type LeaseState string

const (
	LeaseClaimed  LeaseState = "claimed"
	LeaseReported LeaseState = "reported"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

type Verdict string

const (
	VerdictConsistent     Verdict = "consistent"
	VerdictReviewRequired Verdict = "review_required"
)

type EntryType string

const (
	EntryA     EntryType = "A"
	EntryAAAA  EntryType = "AAAA"
	EntryCNAME EntryType = "CNAME"
)

type ApprovalRole string

const (
	RoleReviewer ApprovalRole = "reviewer"
	RoleApprover ApprovalRole = "approver"
)

type ApprovalDecision string

const (
	DecisionApproved ApprovalDecision = "approved"
	DecisionRejected ApprovalDecision = "rejected"
)

type ApprovalStatus string

const (
	ApprovalActive    ApprovalStatus = "active"
	ApprovalWithdrawn ApprovalStatus = "withdrawn"
)

type ZoneEntry struct {
	Owner string    `json:"owner"`
	Type  EntryType `json:"type"`
	Value string    `json:"value"`
}

type ZoneRevision struct {
	ID         string      `json:"id"`
	PlanID     string      `json:"plan_id"`
	Serial     uint32      `json:"serial"`
	Authority  string      `json:"authority"`
	Entries    []ZoneEntry `json:"entries"`
	EntryCount int         `json:"entry_count"`
	CreatedAt  time.Time   `json:"created_at"`
}

type RolloutPlan struct {
	ID                   string     `json:"id"`
	Zone                 string     `json:"zone"`
	State                PlanState  `json:"state"`
	MinimumSuccessRatio  float64    `json:"minimum_success_ratio"`
	MaximumLatencyMS     int        `json:"maximum_latency_ms"`
	ExpectedNodes        int        `json:"expected_nodes"`
	Version              uint64     `json:"version"`
	RequiresDualApproval bool       `json:"requires_dual_approval"`
	LatestRevisionID     string     `json:"latest_revision_id,omitempty"`
	LatestAssessmentID   string     `json:"latest_assessment_id,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	ActivatedAt          *time.Time `json:"activated_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
}

type ResolverNode struct {
	ID        string    `json:"id"`
	NodeName  string    `json:"node_name"`
	Region    string    `json:"region"`
	State     NodeState `json:"state"`
	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type NodeLease struct {
	ID           string     `json:"id"`
	PlanID       string     `json:"plan_id"`
	NodeID       string     `json:"node_id"`
	AllocationID string     `json:"allocation_id"`
	State        LeaseState `json:"state"`
	ClaimedAt    time.Time  `json:"claimed_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ProbeID      string     `json:"probe_id,omitempty"`
}

type LeaseGroup struct {
	ID                 string    `json:"id"`
	PlanID             string    `json:"plan_id"`
	RequestKey         string    `json:"request_key"`
	RequestFingerprint string    `json:"request_fingerprint"`
	Region             string    `json:"region,omitempty"`
	LeaseIDs           []string  `json:"lease_ids"`
	CreatedAt          time.Time `json:"created_at"`
}

type Probe struct {
	ID                 string    `json:"id"`
	LeaseID            string    `json:"lease_id"`
	PlanID             string    `json:"plan_id"`
	NodeID             string    `json:"node_id"`
	RequestKey         string    `json:"request_key"`
	Success            bool      `json:"success"`
	ObservedSerial     uint32    `json:"observed_serial"`
	LatencyMS          int       `json:"latency_ms"`
	ObservedAt         time.Time `json:"observed_at"`
	StoredAt           time.Time `json:"stored_at"`
	PayloadFingerprint string    `json:"payload_fingerprint"`
}

type RolloutAssessment struct {
	ID               string    `json:"id"`
	PlanID           string    `json:"plan_id"`
	RevisionID       string    `json:"revision_id"`
	RequestKey       string    `json:"request_key"`
	Verdict          Verdict   `json:"verdict"`
	Reasons          []string  `json:"reasons"`
	ProbeCount       int       `json:"probe_count"`
	SuccessCount     int       `json:"success_count"`
	SuccessRatio     float64   `json:"success_ratio"`
	MaximumLatencyMS int       `json:"maximum_latency_ms"`
	CreatedAt        time.Time `json:"created_at"`
}

// PlanApproval is one immutable binding of a reviewer's decision to the
// exact plan version and zone revision content that was reviewed. The only
// mutation a stored approval admits is withdrawal of a still-active decision.
type PlanApproval struct {
	ID                  string           `json:"id"`
	PlanID              string           `json:"plan_id"`
	PlanVersion         uint64           `json:"plan_version"`
	RevisionID          string           `json:"revision_id"`
	RevisionSerial      uint32           `json:"revision_serial"`
	RevisionFingerprint string           `json:"revision_fingerprint"`
	Role                ApprovalRole     `json:"role"`
	ReviewerID          string           `json:"reviewer_id"`
	Decision            ApprovalDecision `json:"decision"`
	Note                string           `json:"note,omitempty"`
	RequestKey          string           `json:"request_key"`
	Fingerprint         string           `json:"fingerprint"`
	Status              ApprovalStatus   `json:"status"`
	CreatedAt           time.Time        `json:"created_at"`
	ExpiresAt           time.Time        `json:"expires_at"`
	WithdrawnAt         *time.Time       `json:"withdrawn_at,omitempty"`
}

// ApprovalView is the read projection of an approval with its validity
// evaluated against the current plan at a given instant.
type ApprovalView struct {
	PlanApproval
	Current         bool   `json:"current"`
	Effective       bool   `json:"effective"`
	IneffectiveCode string `json:"ineffective_code,omitempty"`
}

// ApprovalSummary describes whether a plan can be activated under a
// dual-approval policy and which approvals back that answer.
type ApprovalSummary struct {
	PlanID           string         `json:"plan_id"`
	PlanVersion      uint64         `json:"plan_version"`
	RevisionID       string         `json:"revision_id,omitempty"`
	Required         bool           `json:"required"`
	RequiredRoles    []ApprovalRole `json:"required_roles,omitempty"`
	Ready            bool           `json:"ready"`
	MissingRoles     []ApprovalRole `json:"missing_roles,omitempty"`
	Reasons          []string       `json:"reasons,omitempty"`
	Approvals        []ApprovalView `json:"approvals"`
	ActiveRejections []string       `json:"active_rejections,omitempty"`
}

// ApprovalEvaluation is the result of evaluating an activation against the
// approval ledger. A zero evaluation with Ready == true means the ledger
// proves a still-valid dual approval for the requested version.
type ApprovalEvaluation struct {
	Ready        bool
	MissingRoles []ApprovalRole
	Reasons      []string
}

type ApprovalPolicy struct {
	RequiredRoles []ApprovalRole
	TTL           time.Duration
}

func (p ApprovalPolicy) RequiresRole(role ApprovalRole) bool {
	for _, required := range p.RequiredRoles {
		if required == role {
			return true
		}
	}
	return false
}

type AuditEvent struct {
	ID         string         `json:"id"`
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	Action     string         `json:"action"`
	At         time.Time      `json:"at"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type State struct {
	SchemaVersion         int                          `json:"schema_version"`
	Revision              uint64                       `json:"revision"`
	Plans                 map[string]RolloutPlan       `json:"plans"`
	Revisions             map[string]ZoneRevision      `json:"revisions"`
	Nodes                 map[string]ResolverNode      `json:"nodes"`
	Leases                map[string]NodeLease         `json:"leases"`
	LeaseGroups           map[string]LeaseGroup        `json:"lease_groups"`
	Probes                map[string]Probe             `json:"probes"`
	Assessments           map[string]RolloutAssessment `json:"assessments"`
	Approvals             map[string]PlanApproval      `json:"approvals"`
	LeaseRequestKeys      map[string]string            `json:"lease_request_keys"`
	AssessmentRequestKeys map[string]string            `json:"assessment_request_keys"`
	ApprovalRequestKeys   map[string]string            `json:"approval_request_keys"`
	AuditEvents           []AuditEvent                 `json:"audit_events"`
}

type PageMeta struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type PlanPage struct {
	Items []RolloutPlan `json:"items"`
	Meta  PageMeta      `json:"meta"`
}

type NodePage struct {
	Items []ResolverNode `json:"items"`
	Meta  PageMeta       `json:"meta"`
}
