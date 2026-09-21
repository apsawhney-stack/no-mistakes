package types

// Finding ledger protocol version.
const FindingLedgerProtocolVersion = "v1"

// Finding ledger entry status constants.
const (
	FindingLedgerStatusOpen                = "open"
	FindingLedgerStatusPendingVerification = "pending_verification"
	FindingLedgerStatusClosedVerified      = "closed_verified"
	FindingLedgerStatusClosedAccepted      = "closed_accepted"
	FindingLedgerStatusClosedNotApplicable = "closed_not_applicable"
	FindingLedgerStatusClosedSuperseded    = "closed_superseded"
	FindingLedgerStatusNeedsReconciliation = "needs_reconciliation"
)

// Finding ledger event type constants.
const (
	FindingEventAdmitted                   = "admitted"
	FindingEventReportedAgain              = "reported_again"
	FindingEventNotRediscovered            = "not_rediscovered"
	FindingEventSelectedForCorrection      = "selected_for_correction"
	FindingEventCorrectionRevisionRecorded = "correction_revision_recorded"
	FindingEventClosureReviewed            = "closure_reviewed"
	FindingEventAccepted                   = "accepted"
	FindingEventNotApplicable              = "not_applicable"
	FindingEventSuperseded                 = "superseded"
	FindingEventNeedsReconciliation        = "needs_reconciliation"
	FindingEventGenerationInvalidated      = "generation_invalidated"
)

// IsUnresolvedLedgerStatus reports whether a ledger entry status is unresolved
// and thus blocks or requires verification/reconciliation.
func IsUnresolvedLedgerStatus(status string) bool {
	switch status {
	case FindingLedgerStatusOpen, FindingLedgerStatusPendingVerification, FindingLedgerStatusNeedsReconciliation:
		return true
	default:
		return false
	}
}

// IsClosedLedgerStatus reports whether a ledger entry status represents an
// explicitly disposed or verified closed finding.
func IsClosedLedgerStatus(status string) bool {
	switch status {
	case FindingLedgerStatusClosedVerified, FindingLedgerStatusClosedAccepted,
		FindingLedgerStatusClosedNotApplicable, FindingLedgerStatusClosedSuperseded:
		return true
	default:
		return false
	}
}

// FindingLedgerEntry represents one durable finding tracked across execution rounds.
type FindingLedgerEntry struct {
	ID                    string   `json:"id"`
	RunID                 string   `json:"run_id"`
	RepoID                string   `json:"repo_id"`
	StepName              StepName `json:"step_name"`
	FirstSeenRound        int      `json:"first_seen_round"`
	FirstSeenStepResultID string   `json:"first_seen_step_result_id"`
	ReportedID            string   `json:"reported_id"`
	Fingerprint           string   `json:"fingerprint"`
	Severity              string   `json:"severity"`
	Action                string   `json:"action"`
	File                  string   `json:"file,omitempty"`
	Line                  int      `json:"line,omitempty"`
	Description           string   `json:"description"`
	Category              string   `json:"category,omitempty"`
	Check                 string   `json:"check,omitempty"`
	CheckID               string   `json:"check_id,omitempty"`
	DecisionID            string   `json:"decision_id,omitempty"`
	Source                string   `json:"source,omitempty"`
	UserInstructions      string   `json:"user_instructions,omitempty"`
	ReviewScope           string   `json:"review_scope,omitempty"`
	OriginalFindingJSON   string   `json:"original_finding_json"`
	CurrentFindingJSON    string   `json:"current_finding_json"`
	Status                string   `json:"status"`
	IsBlocking            bool     `json:"is_blocking"`
	SelectedInRound       int      `json:"selected_in_round,omitempty"`
	CorrectingCommitSHA   string   `json:"correcting_commit_sha,omitempty"`
	FixSessionID          string   `json:"fix_session_id,omitempty"`
	ClosedInRound         int      `json:"closed_in_round,omitempty"`
	ClosureEvidence       string   `json:"closure_evidence,omitempty"`
	ClosureReason         string   `json:"closure_reason,omitempty"`
	DispositionProvenance string   `json:"disposition_provenance,omitempty"`
	GenerationID          string   `json:"generation_id,omitempty"`
	ClosureGenerationID   string   `json:"closure_generation_id,omitempty"`
	LastObservedRound     int      `json:"last_observed_round"`
	LastObservedFile      string   `json:"last_observed_file,omitempty"`
	LastObservedLine      int      `json:"last_observed_line,omitempty"`
	CreatedAt             int64    `json:"created_at"`
	UpdatedAt             int64    `json:"updated_at"`
}

// FindingLedgerEvent represents a single observation, state transition, or disposition event.
type FindingLedgerEvent struct {
	ID           string   `json:"id"`
	EntryID      string   `json:"entry_id"`
	RunID        string   `json:"run_id"`
	StepName     StepName `json:"step_name"`
	Round        int      `json:"round"`
	StepResultID string   `json:"step_result_id,omitempty"`
	EventType    string   `json:"event_type"`
	StateBefore  string   `json:"state_before"`
	StateAfter   string   `json:"state_after"`
	CommitSHA    string   `json:"commit_sha,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Evidence     string   `json:"evidence,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Provenance   string   `json:"provenance,omitempty"`
	GenerationID string   `json:"generation_id,omitempty"`
	CreatedAt    int64    `json:"created_at"`
}

// FindingLedgerSummary is the versioned, publishable view of one run's durable
// finding ledger. Counts cover every entry ever admitted; Entries carries only
// the unresolved ones (open, pending verification, reconciliation required),
// because that is what a gate, a status reader, or a terminal-acceptance check
// needs and because it bounds what a summary may embed in a step's findings JSON
// or an IPC frame. Closed entries remain readable per-run through the database
// and are counted, never dropped, here.
type FindingLedgerSummary struct {
	ProtocolVersion string                      `json:"protocol_version"`
	RunID           string                      `json:"run_id"`
	TotalEntries    int                         `json:"total_entries"`
	OpenCount       int                         `json:"open_count"`
	PendingCount    int                         `json:"pending_count"`
	ReconcileCount  int                         `json:"reconcile_count"`
	ClosedCount     int                         `json:"closed_count"`
	HasBlocking     bool                        `json:"has_blocking"`
	Entries         []FindingLedgerEntrySummary `json:"entries,omitempty"`
}

// Unresolved returns the number of entries the protocol still requires a
// disposition for.
func (s *FindingLedgerSummary) Unresolved() int {
	if s == nil {
		return 0
	}
	return s.OpenCount + s.PendingCount + s.ReconcileCount
}

// FindingLedgerEntrySummary is a compact representation of a ledger entry in protocol summaries.
type FindingLedgerEntrySummary struct {
	ID                  string   `json:"id"`
	StepName            StepName `json:"step_name"`
	ReportedID          string   `json:"reported_id"`
	Severity            string   `json:"severity"`
	Action              string   `json:"action"`
	File                string   `json:"file,omitempty"`
	Line                int      `json:"line,omitempty"`
	Description         string   `json:"description"`
	Status              string   `json:"status"`
	IsBlocking          bool     `json:"is_blocking"`
	FirstSeenRound      int      `json:"first_seen_round"`
	LastObservedRound   int      `json:"last_observed_round"`
	CorrectingCommitSHA string   `json:"correcting_commit_sha,omitempty"`
	ClosureReason       string   `json:"closure_reason,omitempty"`
}
