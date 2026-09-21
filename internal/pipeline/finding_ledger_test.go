package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func setupTestDB(t *testing.T) (*db.DB, string, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repo, err := database.InsertRepo(dir, "git@github.com:owner/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	return database, run.ID, repo.ID
}

func mustMarshalFindings(f types.Findings) string {
	s, _ := types.MarshalFindingsJSON(f)
	return s
}

func TestFindingLedger_OriginalFailureShapeOmissionDoesNotClearFindings(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	// Round 1: Analyzer reports 5 blocking findings (F1..F5).
	r1Findings := types.Findings{
		Summary: "5 findings reported",
		Items: []types.Finding{
			{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, File: "a.go", Line: 10, Description: "defect 1"},
			{ID: "review-2", Severity: "error", Action: types.ActionAutoFix, File: "b.go", Line: 20, Description: "defect 2"},
			{ID: "review-3", Severity: "warning", Action: types.ActionAskUser, File: "c.go", Line: 30, Description: "defect 3"},
			{ID: "review-4", Severity: "error", Action: types.ActionAutoFix, File: "d.go", Line: 40, Description: "defect 4"},
			{ID: "review-5", Severity: "error", Action: types.ActionAutoFix, File: "d.go", Line: 50, Description: "defect 5"},
		},
	}
	r1Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r1Findings),
		ReviewablePaths: []string{"a.go", "b.go", "c.go", "d.go"},
		ReviewedPaths:   []string{"a.go", "b.go", "c.go", "d.go"},
	}

	effectiveR1JSON, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, false, "head-1", "")
	if err != nil {
		t.Fatalf("process round 1: %v", err)
	}
	parsedR1, err := types.ParseFindingsJSON(effectiveR1JSON)
	if err != nil {
		t.Fatalf("parse effective r1: %v", err)
	}
	if len(parsedR1.Items) != 5 {
		t.Fatalf("expected 5 items in effective R1 findings, got %d", len(parsedR1.Items))
	}

	// Selection: Operator / auto-fix selects only F4 and F5 for correction.
	err = ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"review-4", "review-5"}, "auto_fix")
	if err != nil {
		t.Fatalf("process selection: %v", err)
	}

	// Fix commit created:
	fixCommitSHA := "commit-fix-d"
	err = ledger.RecordCorrectingRevision(ctx, types.StepReview, 1, fixCommitSHA, "fixer-session-1")
	if err != nil {
		t.Fatalf("record correcting revision: %v", err)
	}

	// Round 2: Re-review verifies d.go (covered, defect 4 and 5 gone).
	// Crucially: re-review omits a.go, b.go, c.go entirely!
	r2Findings := types.Findings{
		Summary: "clean on d.go",
		Items:   nil,
	}
	r2Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r2Findings),
		ReviewablePaths: []string{"a.go", "b.go", "c.go", "d.go"},
		ReviewedPaths:   []string{"d.go"},
	}

	effectiveR2JSON, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Outcome, false, fixCommitSHA, "reviewer-session-2")
	if err != nil {
		t.Fatalf("process round 2: %v", err)
	}

	parsedR2, err := types.ParseFindingsJSON(effectiveR2JSON)
	if err != nil {
		t.Fatalf("parse effective r2: %v", err)
	}

	// F1, F2, F3 MUST still be present in the effective findings!
	if len(parsedR2.Items) != 3 {
		t.Fatalf("expected 3 remaining unaddressed findings (F1..F3), got %d", len(parsedR2.Items))
	}

	// Verify ledger entries in DB
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 total ledger entries, got %d", len(entries))
	}

	for _, e := range entries {
		switch e.ReportedID {
		case "review-4", "review-5":
			if e.Status != types.FindingLedgerStatusClosedVerified {
				t.Errorf("entry %s status = %s, want closed_verified", e.ReportedID, e.Status)
			}
			if e.CorrectingCommitSHA != fixCommitSHA {
				t.Errorf("entry %s commit = %s, want %s", e.ReportedID, e.CorrectingCommitSHA, fixCommitSHA)
			}
		case "review-1", "review-2", "review-3":
			if e.Status != types.FindingLedgerStatusOpen {
				t.Errorf("entry %s status = %s, want open", e.ReportedID, e.Status)
			}
			if !e.IsBlocking {
				t.Errorf("entry %s should still be blocking", e.ReportedID)
			}
		default:
			t.Errorf("unexpected entry %s", e.ReportedID)
		}
	}

	// Verify events log: F1..F3 must have a not_rediscovered event
	events, err := database.GetFindingLedgerEventsByRun(runID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	notRediscoveredCount := 0
	closureReviewedCount := 0
	for _, ev := range events {
		if ev.EventType == types.FindingEventNotRediscovered {
			notRediscoveredCount++
		}
		if ev.EventType == types.FindingEventClosureReviewed {
			closureReviewedCount++
		}
	}
	if notRediscoveredCount != 3 {
		t.Errorf("expected 3 not_rediscovered events, got %d", notRediscoveredCount)
	}
	if closureReviewedCount != 2 {
		t.Errorf("expected 2 closure_reviewed events, got %d", closureReviewedCount)
	}

	// Terminal acceptance MUST be refused because F1..F3 remain open and blocking!
	err = ledger.AssertAcceptance(ctx)
	if err == nil {
		t.Fatalf("AssertAcceptance should have refused terminal acceptance with open blocking findings")
	}

	// Operator now explicitly accepts remaining findings at approval gate:
	err = ledger.ProcessExplicitDisposition(ctx, types.StepReview, sr.ID, 2, types.ActionApprove, "waived by operator", "operator_approval")
	if err != nil {
		t.Fatalf("process explicit disposition: %v", err)
	}

	// Now terminal acceptance must succeed!
	err = ledger.AssertAcceptance(ctx)
	if err != nil {
		t.Fatalf("AssertAcceptance failed after explicit disposition: %v", err)
	}

	// Stats verify: FixedFindings must strictly be 2 (the closed_verified ones), NOT 5!
	stats, err := database.StepFindingStats(sr)
	if err != nil {
		t.Fatalf("get step stats: %v", err)
	}
	if stats.FixedFindings != 2 {
		t.Errorf("stats.FixedFindings = %d, want 2 (only verified fixes)", stats.FixedFindings)
	}
	if stats.ReportedFindings != 5 {
		t.Errorf("stats.ReportedFindings = %d, want 5", stats.ReportedFindings)
	}
}

func TestFindingLedger_UnambiguousLineShiftPreservesIdentity(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	// Round 1: Defect at main.go:20
	r1Findings := types.Findings{
		Items: []types.Finding{
			{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, File: "main.go", Line: 20, Description: "nil pointer dereference on user input"},
		},
	}
	r1Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r1Findings),
		ReviewablePaths: []string{"main.go"},
		ReviewedPaths:   []string{"main.go"},
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, false, "h1", "")
	if err != nil {
		t.Fatalf("r1: %v", err)
	}

	entries1, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil || len(entries1) != 1 {
		t.Fatalf("expected 1 entry, got %v (err: %v)", len(entries1), err)
	}
	originalEntryID := entries1[0].ID

	// Round 2: Same defect, but code inserted earlier moved it to line 45
	r2Findings := types.Findings{
		Items: []types.Finding{
			{ID: "review-1-new", Severity: "error", Action: types.ActionAutoFix, File: "main.go", Line: 45, Description: "nil pointer dereference on user input"},
		},
	}
	r2Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r2Findings),
		ReviewablePaths: []string{"main.go"},
		ReviewedPaths:   []string{"main.go"},
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Outcome, false, "h2", "")
	if err != nil {
		t.Fatalf("r2: %v", err)
	}

	entries2, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil || len(entries2) != 1 {
		t.Fatalf("expected 1 entry preserved across line shift, got %d", len(entries2))
	}

	e := entries2[0]
	if e.ID != originalEntryID {
		t.Errorf("entry ID changed from %s to %s across line shift", originalEntryID, e.ID)
	}
	if e.LastObservedLine != 45 {
		t.Errorf("entry LastObservedLine = %d, want 45", e.LastObservedLine)
	}
	if e.LastObservedRound != 2 {
		t.Errorf("entry LastObservedRound = %d, want 2", e.LastObservedRound)
	}

	// Verify reported_again event recorded
	events, err := database.GetFindingLedgerEventsByRun(runID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	hasReportedAgain := false
	for _, ev := range events {
		if ev.EventType == types.FindingEventReportedAgain && ev.EntryID == originalEntryID {
			hasReportedAgain = true
		}
	}
	if !hasReportedAgain {
		t.Errorf("missing reported_again event for line shift")
	}
}

func TestFindingLedger_AmbiguousMatchesRemainDistinct(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	// Round 1: Two distinct findings in the same file with identical message
	r1Findings := types.Findings{
		Items: []types.Finding{
			{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "util.go", Line: 10, Description: "unhandled error"},
			{ID: "f-2", Severity: "error", Action: types.ActionAutoFix, File: "util.go", Line: 50, Description: "unhandled error"},
		},
	}
	r1Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r1Findings),
		ReviewablePaths: []string{"util.go"},
		ReviewedPaths:   []string{"util.go"},
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, false, "h1", "")
	if err != nil {
		t.Fatalf("r1: %v", err)
	}

	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 distinct entries, got %d", len(entries))
	}
	if entries[0].ID == entries[1].ID {
		t.Fatalf("ambiguous findings merged into identical ID %s", entries[0].ID)
	}
}

func TestFindingLedger_VerifiedFixClosureRequirements(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	// Round 1: Finding admitted
	r1Findings := types.Findings{
		Items: []types.Finding{
			{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "worker.go", Line: 25, Description: "race condition"},
		},
	}
	r1Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r1Findings),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"worker.go"},
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, false, "h1", "")
	if err != nil {
		t.Fatalf("r1: %v", err)
	}

	// Select for fix
	_ = ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, "auto_fix")

	// Case 1: No commit SHA recorded -> cannot close even if omitted in round 2
	r2Empty := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"worker.go"},
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Empty, false, "", "reviewer")
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified without correcting revision commit SHA")
	}

	// Record correcting revision with fixer session
	_ = ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "commit-fix-1", "fixer-session-A")

	// Case 2: Fixer session itself cannot self-certify closure
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 3, r2Empty, false, "commit-fix-1", "fixer-session-A")
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified by same session that applied fix (self-certification)")
	}

	// Case 3: Independent reviewer with unreviewed/uncovered file cannot certify closure
	r4Uncovered := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"other.go"}, // worker.go not covered!
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 4, r4Uncovered, false, "commit-fix-1", "reviewer-session-B")
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified when review did not cover the affected file")
	}

	// Case 4: Independent reviewer verifies worker.go -> SUCCESSFUL closure!
	r5Covered := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"worker.go"},
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 5, r5Covered, false, "commit-fix-1", "reviewer-session-B")
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status != types.FindingLedgerStatusClosedVerified {
		t.Fatalf("expected closed_verified, got %s", entries[0].Status)
	}

	// Acceptance now passes
	if err := ledger.AssertAcceptance(ctx); err != nil {
		t.Fatalf("AssertAcceptance failed after verified closure: %v", err)
	}
}

func TestFindingLedger_ExplicitDispositions(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	r1Findings := types.Findings{
		Items: []types.Finding{
			{ID: "f-1", Severity: "warning", Action: types.ActionAskUser, File: "a.go", Line: 10, Description: "style issue"},
			{ID: "f-2", Severity: "warning", Action: types.ActionAskUser, File: "b.go", Line: 20, Description: "legacy pattern"},
		},
	}
	r1Outcome := &StepOutcome{
		Findings:        mustMarshalFindings(r1Findings),
		ReviewablePaths: []string{"a.go", "b.go"},
		ReviewedPaths:   []string{"a.go", "b.go"},
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, false, "h1", "")

	// Dispose f-1 as not_applicable
	err = ledger.ProcessExplicitDispositionForEntries(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, types.FindingLedgerStatusClosedNotApplicable, "false positive on generated code", "human_supervisor")
	if err != nil {
		t.Fatalf("dispose not_applicable: %v", err)
	}

	// Dispose f-2 as superseded
	err = ledger.ProcessExplicitDispositionForEntries(ctx, types.StepReview, sr.ID, 1, []string{"f-2"}, types.FindingLedgerStatusClosedSuperseded, "replaced by architecture decision 42", "human_supervisor")
	if err != nil {
		t.Fatalf("dispose superseded: %v", err)
	}

	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	for _, e := range entries {
		if e.ReportedID == "f-1" && e.Status != types.FindingLedgerStatusClosedNotApplicable {
			t.Errorf("f-1 status = %s, want closed_not_applicable", e.Status)
		}
		if e.ReportedID == "f-2" && e.Status != types.FindingLedgerStatusClosedSuperseded {
			t.Errorf("f-2 status = %s, want closed_superseded", e.Status)
		}
	}

	// Acceptance passes because both are closed
	if err := ledger.AssertAcceptance(ctx); err != nil {
		t.Fatalf("AssertAcceptance: %v", err)
	}
}

func TestFindingLedger_TerminalAcceptanceRefusesNeedsReconciliation(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}

	// Insert entry in needs_reconciliation state (as created during legacy active run migration)
	entry := &types.FindingLedgerEntry{
		ID:                    "fn-reconcile-1",
		RunID:                 runID,
		RepoID:                repoID,
		StepName:              types.StepReview,
		FirstSeenRound:        1,
		FirstSeenStepResultID: sr.ID,
		ReportedID:            "rev-1",
		Fingerprint:           "fp-rev-1",
		Severity:              "error",
		Action:                types.ActionAutoFix,
		File:                  "main.go",
		Line:                  10,
		Description:           "unverified omission from pre-ledger active run",
		OriginalFindingJSON:   `{}`,
		CurrentFindingJSON:    `{}`,
		Status:                types.FindingLedgerStatusNeedsReconciliation,
		IsBlocking:            true,
		LastObservedRound:     1,
		LastObservedFile:      "main.go",
		LastObservedLine:      10,
	}
	if err := database.InsertFindingLedgerEntry(entry); err != nil {
		t.Fatalf("insert entry: %v", err)
	}

	// Acceptance MUST be refused
	err = ledger.AssertAcceptance(ctx)
	if err == nil {
		t.Fatalf("AssertAcceptance should have refused with needs_reconciliation entry")
	}

	// Operator reconciles / resolves it
	err = ledger.ProcessExplicitDisposition(ctx, types.StepReview, sr.ID, 2, types.ActionApprove, "reconciled by operator", "operator")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Acceptance now succeeds
	if err := ledger.AssertAcceptance(ctx); err != nil {
		t.Fatalf("AssertAcceptance failed after reconciliation: %v", err)
	}
}
