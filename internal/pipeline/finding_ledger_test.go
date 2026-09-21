package pipeline

import (
	"context"
	"path/filepath"
	"strconv"
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

	effectiveR1JSON, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, "head-1")
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
		// A fresh, independent closure turn: not the fixer session.
		AgentSessionID: "reviewer-session-2",
	}

	effectiveR2JSON, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Outcome, fixCommitSHA)
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
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, "h1")
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
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Outcome, "h2")
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
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, "h1")
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
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, "h1")
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
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, r2Empty, "")
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified without correcting revision commit SHA")
	}

	// Record correcting revision with fixer session
	_ = ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "commit-fix-1", "fixer-session-A")

	// Case 2: Fixer session itself cannot self-certify closure
	selfCertifying := &StepOutcome{
		Findings:            mustMarshalFindings(types.Findings{}),
		ReviewablePaths:     []string{"worker.go"},
		ReviewedPaths:       []string{"worker.go"},
		AgentSessionID:      "fixer-session-A",
		AgentSessionResumed: true,
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 3, selfCertifying, "commit-fix-1")
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified by same session that applied fix (self-certification)")
	}

	// Case 3: Independent reviewer with unreviewed/uncovered file cannot certify closure
	r4Uncovered := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"other.go"}, // worker.go not covered!
		AgentSessionID:  "reviewer-session-B",
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 4, r4Uncovered, "commit-fix-1")
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatalf("closed verified when review did not cover the affected file")
	}

	// Case 4: Independent reviewer verifies worker.go -> SUCCESSFUL closure!
	r5Covered := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"worker.go"},
		AgentSessionID:  "reviewer-session-B",
	}
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 5, r5Covered, "commit-fix-1")
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
	_, _ = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, r1Outcome, "h1")

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

// TestFindingLedger_PreservesRoundEvidenceMetadata reproduces the data-loss
// shape the ledger must never have: a round that reports no findings still owns
// its evidence, and an empty payload would erase it.
//
// Two concrete consequences are covered: a green Test step must keep
// publishing its live-validation verdict (the PR attestation's live_validation
// reads it), and a clean review round must keep publishing its coverage record
// (a missing one parks the gate).
func TestFindingLedger_PreservesRoundEvidenceMetadata(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	testStep, err := database.InsertStepResult(runID, types.StepTest)
	if err != nil {
		t.Fatalf("insert test step result: %v", err)
	}
	testOutcome := &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{
			Verdict:       types.TestVerdictGo,
			TestedHeadSHA: "abc123",
			Scenarios:     []types.TestScenario{{Name: "scenario", Result: "pass", Live: true, Evidence: "ran it"}},
			RiskLevel:     "low",
		}),
		ExitCode: 0,
	}
	effective, err := ledger.ProcessRoundFindings(ctx, types.StepTest, testStep.ID, 1, testOutcome, "abc123")
	if err != nil {
		t.Fatalf("process test round: %v", err)
	}
	if effective == "" {
		t.Fatal("green Test round produced an empty payload, erasing its verdict")
	}
	parsedTest, err := types.ParseFindingsJSON(effective)
	if err != nil {
		t.Fatalf("parse test payload: %v", err)
	}
	if parsedTest.Verdict != types.TestVerdictGo || parsedTest.TestedHeadSHA != "abc123" || len(parsedTest.Scenarios) != 1 {
		t.Fatalf("test evidence lost: verdict=%q head=%q scenarios=%d", parsedTest.Verdict, parsedTest.TestedHeadSHA, len(parsedTest.Scenarios))
	}

	reviewStep, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert review step result: %v", err)
	}
	reviewOutcome := &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{RiskLevel: "low"}),
		ReviewedPaths:   []string{"a.go"},
		ReviewablePaths: []string{"a.go"},
	}
	reviewEffective, err := ledger.ProcessRoundFindings(ctx, types.StepReview, reviewStep.ID, 1, reviewOutcome, "abc123")
	if err != nil {
		t.Fatalf("process review round: %v", err)
	}
	if reviewEffective == "" {
		t.Fatal("clean review round produced an empty payload, erasing its coverage record")
	}
	parsedReview, err := types.ParseFindingsJSON(reviewEffective)
	if err != nil {
		t.Fatalf("parse review payload: %v", err)
	}
	if len(parsedReview.ReviewedPaths) != 1 || parsedReview.ReviewedPaths[0] != "a.go" {
		t.Fatalf("review coverage lost: %+v", parsedReview.ReviewedPaths)
	}
}

// TestFindingLedger_ClosureRequiresTheCorrectedRevision pins that a closure
// review must be a review OF the correction. Coverage of the right file on some
// other head says nothing about the fix.
func TestFindingLedger_ClosureRequiresTheCorrectedRevision(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "worker.go", Line: 3, Description: "data race"},
		}}),
		ReviewablePaths: []string{"worker.go"},
		ReviewedPaths:   []string{"worker.go"},
	}, "h1")
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if err := ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "corrected-head", "fixer"); err != nil {
		t.Fatalf("record correction: %v", err)
	}

	covered := func(round int, head string) *StepOutcome {
		return &StepOutcome{
			Findings:        mustMarshalFindings(types.Findings{}),
			ReviewablePaths: []string{"worker.go"},
			ReviewedPaths:   []string{"worker.go"},
			AgentSessionID:  "independent-reviewer",
		}
	}
	// A review of a head that is not the corrected revision cannot close it.
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 3, covered(3, "some-later-head"), "some-later-head"); err != nil {
		t.Fatalf("r3: %v", err)
	}
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatal("closed verified from a review of a different revision")
	}
	// The review of the corrected revision does.
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 4, covered(4, "corrected-head"), "corrected-head"); err != nil {
		t.Fatalf("r4: %v", err)
	}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status != types.FindingLedgerStatusClosedVerified {
		t.Fatalf("status = %q, want closed_verified", entries[0].Status)
	}
	events, err := database.GetFindingLedgerEventsByRun(runID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	sawClosure := false
	for _, ev := range events {
		if ev.EventType == types.FindingEventClosureReviewed && ev.CommitSHA == "corrected-head" && ev.SessionID == "independent-reviewer" {
			sawClosure = true
		}
	}
	if !sawClosure {
		t.Fatal("closure event did not record the correcting revision and the certifying session")
	}
}

// TestFindingLedger_StepOwnedFindingsStayOutOfTheLedger pins that an operator
// park the step re-derives from live state is carried on the round's payload
// but never admitted, so the gate still refuses while nothing has to close it.
func TestFindingLedger_StepOwnedFindingsStayOutOfTheLedger(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepTest)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	outcome := &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: types.FindingIDTestAgentTimeout, Severity: "warning", Action: types.ActionAskUser, Description: "budget cut"},
			{ID: types.FindingIDTestAgentUnvalidatedWork, Severity: "error", Action: types.ActionAskUser, Description: "unvalidated work"},
			{ID: "real-1", Severity: "error", Action: types.ActionAutoFix, File: "a.go", Description: "scenario failed"},
		}}),
		ReviewedPaths:   []string{"a.go"},
		ReviewablePaths: []string{"a.go"},
	}
	effective, err := ledger.ProcessRoundFindings(ctx, types.StepTest, sr.ID, 1, outcome, "head-1")
	if err != nil {
		t.Fatalf("process round: %v", err)
	}
	parsed, err := types.ParseFindingsJSON(effective)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Items) != 3 {
		t.Fatalf("effective findings = %d items, want all 3 visible at the gate", len(parsed.Items))
	}
	if pipelineHasID(parsed.Items, types.FindingIDTestAgentTimeout) == false {
		t.Fatal("step-owned budget cut dropped from the gate payload")
	}
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepTest)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 1 || entries[0].ReportedID != "real-1" {
		t.Fatalf("ledger admitted step-owned findings: %+v", entries)
	}
	if parsed.Ledger == nil || parsed.Ledger.OpenCount != 1 {
		t.Fatalf("summary = %+v, want exactly the one analyzer finding", parsed.Ledger)
	}
	if parsed.Ledger.Unresolved() == 0 {
		t.Fatal("an unresolved analyzer finding must keep the gate parked")
	}
	if !ledgerRequiresDisposition(effective) {
		t.Fatal("ledgerRequiresDisposition missed the unresolved analyzer finding")
	}
}

func pipelineHasID(items []types.Finding, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// TestFindingLedger_CISettledObservationSupersedesChecksOnly pins the CI step's
// documented ownership: a fresh settled observation replaces the previous one,
// but only for the checks it actually observed. A review-bot comment is a human
// decision and survives a green check run.
func TestFindingLedger_CISettledObservationSupersedesChecksOnly(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepCI)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	_, err = ledger.ProcessRoundFindings(ctx, types.StepCI, sr.ID, 1, &StepOutcome{
		NeedsApproval: true,
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "ci-1", Severity: "error", Action: types.ActionAskUser, Category: types.FindingCategoryCICheck, Check: "test", Description: "check failing"},
			{ID: "bot-1", Severity: "warning", Action: types.ActionAskUser, Category: types.FindingCategoryCIReviewBot, Description: "bot comment"},
		}}),
		ExitCode: 1,
	}, "head-1")
	if err != nil {
		t.Fatalf("process failing round: %v", err)
	}

	// A non-clean CI round supersedes nothing.
	_, err = ledger.ProcessRoundFindings(ctx, types.StepCI, sr.ID, 2, &StepOutcome{
		NeedsApproval: true,
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "ci-1", Severity: "error", Action: types.ActionAskUser, Category: types.FindingCategoryCICheck, Check: "test", Description: "check failing"},
		}}),
		ExitCode: 1,
	}, "head-1")
	if err != nil {
		t.Fatalf("process second failing round: %v", err)
	}
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepCI)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	// A settled clean observation at the head supersedes the check finding and
	// leaves the review-bot comment outstanding.
	effective, err := ledger.ProcessRoundFindings(ctx, types.StepCI, sr.ID, 3, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{}),
		ExitCode: 0,
	}, "head-2")
	if err != nil {
		t.Fatalf("process clean round: %v", err)
	}
	byReported := map[string]*types.FindingLedgerEntry{}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepCI)
	for _, e := range entries {
		byReported[e.ReportedID] = e
	}
	if got := byReported["ci-1"].Status; got != types.FindingLedgerStatusClosedSuperseded {
		t.Fatalf("check finding status = %q, want closed_superseded", got)
	}
	if got := byReported["ci-1"].DispositionProvenance; got != "ci_settled_observation" {
		t.Fatalf("check finding provenance = %q", got)
	}
	if got := byReported["bot-1"].Status; got != types.FindingLedgerStatusOpen {
		t.Fatalf("review-bot finding status = %q, want it still open", got)
	}
	parsed, err := types.ParseFindingsJSON(effective)
	if err != nil {
		t.Fatalf("parse effective: %v", err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].ID != "bot-1" {
		t.Fatalf("effective findings = %+v, want only the review-bot comment", parsed.Items)
	}
	if !ledgerRequiresDisposition(effective) {
		t.Fatal("an outstanding review-bot comment must keep the gate parked")
	}
	// Supersession is not a verified fix: it must never read as Fixed.
	stats, err := database.StepFindingStats(sr)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.FixedFindings != 0 || stats.ReportedFindings != 2 {
		t.Fatalf("stats = %+v, want 2 reported and 0 verified fixes", stats)
	}
}

// TestFindingLedger_SurvivesRestartAndStaleRevisionCannotCloseIt is the
// restart-equivalent of a crash and daemon restart: a brand-new ledger over the
// same database must see the same entry, keep it blocking, and refuse to let a
// stale correction revision certify it.
func TestFindingLedger_SurvivesRestartAndStaleRevisionCannotCloseIt(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()

	first := NewFindingLedger(database, runID, repoID)
	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	if _, err := first.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "svc.go", Line: 7, Description: "unbounded retry loop"},
		}}),
		ReviewablePaths: []string{"svc.go"},
		ReviewedPaths:   []string{"svc.go"},
	}, "head-a"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := first.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if err := first.RecordCorrectingRevision(ctx, types.StepReview, 2, "head-b", "fixer-session"); err != nil {
		t.Fatalf("record correction: %v", err)
	}

	// Restart: a fresh ledger (fresh process) over the same database.
	second := NewFindingLedger(database, runID, repoID)
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("entries after restart: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != types.FindingLedgerStatusPendingVerification {
		t.Fatalf("entries after restart = %+v", entries)
	}

	// A clean round on the STALE head must not close it.
	if _, err := second.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 3, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"svc.go"},
		ReviewedPaths:   []string{"svc.go"},
		AgentSessionID:  "independent-reviewer",
	}, "head-a"); err != nil {
		t.Fatalf("stale round: %v", err)
	}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatal("a stale revision closed a finding after a restart")
	}
	if err := second.AssertAcceptance(ctx); err == nil {
		t.Fatal("AssertAcceptance accepted a run with a pending verification")
	}

	// The corrected revision certifies it, and the restarted ledger logs the
	// closure against the same entry identity.
	effective, err := second.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 4, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"svc.go"},
		ReviewedPaths:   []string{"svc.go"},
		AgentSessionID:  "independent-reviewer",
	}, "head-b")
	if err != nil {
		t.Fatalf("corrected round: %v", err)
	}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status != types.FindingLedgerStatusClosedVerified {
		t.Fatalf("status = %q, want closed_verified", entries[0].Status)
	}
	if len(entries[0].ID) == 0 {
		t.Fatal("entry identity lost")
	}
	if effective != "" {
		parsed, parseErr := types.ParseFindingsJSON(effective)
		if parseErr != nil {
			t.Fatalf("parse effective: %v", parseErr)
		}
		if len(parsed.Items) != 0 {
			t.Fatalf("closed finding still reported: %+v", parsed.Items)
		}
	}
	if err := second.AssertAcceptance(ctx); err != nil {
		t.Fatalf("AssertAcceptance after verified closure: %v", err)
	}
}

// TestFindingLedger_ReReportResetsCorrectionEvidence pins that a rejected fix
// cannot keep vouching for itself: once the rereview reports the defect again,
// the entry reopens and the superseded revision is forgotten.
func TestFindingLedger_ReReportResetsCorrectionEvidence(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	finding := types.Finding{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "a.go", Line: 4, Description: "leaked handle"}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{Items: []types.Finding{finding}}),
		ReviewablePaths: []string{"a.go"},
		ReviewedPaths:   []string{"a.go"},
	}, "head-a"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if err := ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "head-b", "fixer-session"); err != nil {
		t.Fatalf("record correction: %v", err)
	}

	// The rereview reports the same defect: the fix failed.
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{Items: []types.Finding{finding}}),
		ReviewablePaths: []string{"a.go"},
		ReviewedPaths:   []string{"a.go"},
		AgentSessionID:  "independent-reviewer",
	}, "head-b"); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status != types.FindingLedgerStatusOpen {
		t.Fatalf("status = %q, want open after the fix failed", entries[0].Status)
	}
	if entries[0].CorrectingCommitSHA != "" || entries[0].FixSessionID != "" || entries[0].ClosureEvidence != "" {
		t.Fatalf("stale correction evidence survived a re-report: %+v", entries[0])
	}

	// A later clean round on the old head must not close it on that stale SHA.
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 3, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"a.go"},
		ReviewedPaths:   []string{"a.go"},
		AgentSessionID:  "independent-reviewer",
	}, "head-b"); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if entries[0].Status == types.FindingLedgerStatusClosedVerified {
		t.Fatal("closed verified with no recorded correcting revision after the failed fix")
	}
}

// TestFindingLedger_SubsetSelectionLeavesTheRestBlocking is the explicit
// acceptance counterexample: F1-F5 reported, only F4/F5 selected, F1-F3 omitted
// from the next round. The unselected entries stay visible and blocking.
func TestFindingLedger_SubsetSelectionLeavesTheRestBlocking(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	items := make([]types.Finding, 0, 5)
	for i := 1; i <= 5; i++ {
		items = append(items, types.Finding{
			ID:          "F" + strconv.Itoa(i),
			Severity:    "error",
			Action:      types.ActionAutoFix,
			File:        "p" + strconv.Itoa(i) + ".go",
			Line:        i * 10,
			Description: "defect " + strconv.Itoa(i),
		})
	}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{Items: items}),
		ReviewablePaths: []string{"p1.go", "p2.go", "p3.go", "p4.go", "p5.go"},
		ReviewedPaths:   []string{"p1.go", "p2.go", "p3.go", "p4.go", "p5.go"},
	}, "head-a"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"F4", "F5"}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	entries, _ := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	pending := map[string]string{}
	for _, e := range entries {
		pending[e.ReportedID] = e.Status
	}
	for _, id := range []string{"F1", "F2", "F3"} {
		if pending[id] != types.FindingLedgerStatusOpen {
			t.Fatalf("%s status = %q, want open (unselected)", id, pending[id])
		}
	}
	for _, id := range []string{"F4", "F5"} {
		if pending[id] != types.FindingLedgerStatusPendingVerification {
			t.Fatalf("%s status = %q, want pending_verification", id, pending[id])
		}
	}

	// Round two reports nothing at all. An empty scan closes nothing.
	effective, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{}),
		ReviewablePaths: []string{"p1.go", "p2.go", "p3.go", "p4.go", "p5.go"},
	}, "head-b")
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	parsed, err := types.ParseFindingsJSON(effective)
	if err != nil {
		t.Fatalf("parse effective: %v", err)
	}
	if len(parsed.Items) != 5 {
		t.Fatalf("effective findings = %d items, want all 5 still visible", len(parsed.Items))
	}
	if err := ledger.AssertAcceptance(ctx); err == nil {
		t.Fatal("AssertAcceptance accepted a run with 5 unresolved findings")
	}
	// F4/F5 were never corrected, so they cannot be verified closed either.
	for _, e := range parsed.Items {
		_ = e
	}
	entries, _ = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	for _, e := range entries {
		if e.Status == types.FindingLedgerStatusClosedVerified {
			t.Fatalf("%s closed verified without a recorded correction", e.ReportedID)
		}
	}
}

// TestFindingLedger_SelectionResolvesDisplayIDsNotRawLabels pins the collision
// case: two rounds that both labelled a finding "review-1" produce two distinct
// entries, so the second one's display ID is minted. A selection naming that
// minted display ID must select the entry the operator saw, not whichever entry
// happens to carry the raw label.
func TestFindingLedger_SelectionResolvesDisplayIDsNotRawLabels(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	first := types.Finding{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, File: "a.go", Line: 3, Description: "first defect"}
	second := types.Finding{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, File: "b.go", Line: 8, Description: "second defect"}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{Items: []types.Finding{first}}),
		ReviewablePaths: []string{"a.go", "b.go"},
		ReviewedPaths:   []string{"a.go", "b.go"},
	}, "head-1"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	effective, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, &StepOutcome{
		Findings:        mustMarshalFindings(types.Findings{Items: []types.Finding{second}}),
		ReviewablePaths: []string{"a.go", "b.go"},
		ReviewedPaths:   []string{"a.go", "b.go"},
	}, "head-1")
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	parsed, err := types.ParseFindingsJSON(effective)
	if err != nil {
		t.Fatalf("parse effective: %v", err)
	}
	displayToFile := map[string]string{}
	for _, item := range parsed.Items {
		displayToFile[item.ID] = item.File
	}
	if len(displayToFile) != 2 {
		t.Fatalf("display IDs are not unique: %+v", displayToFile)
	}
	minted := ""
	for id, file := range displayToFile {
		if file == "b.go" {
			minted = id
		}
	}
	if minted == "" || minted == "review-1" {
		t.Fatalf("expected the second entry to get a minted display ID, got %q (%+v)", minted, displayToFile)
	}

	if err := ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 2, []string{minted}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	statusByFile := map[string]string{}
	for _, e := range entries {
		statusByFile[e.File] = e.Status
	}
	if statusByFile["b.go"] != types.FindingLedgerStatusPendingVerification {
		t.Fatalf("b.go status = %q, want pending_verification", statusByFile["b.go"])
	}
	if statusByFile["a.go"] != types.FindingLedgerStatusOpen {
		t.Fatalf("a.go status = %q, want it left open (never selected)", statusByFile["a.go"])
	}
}

// TestFindingLedger_LineShiftKeepsOneIdentityWithBothObservations pins the
// unambiguous moved-line case: one entry, both observations retained.
func TestFindingLedger_LineShiftKeepsOneIdentityWithBothObservations(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	moved := func(line int) types.Finding {
		return types.Finding{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, File: "lib.go", Line: line, Description: "unchecked error"}
	}
	for _, observed := range []struct {
		round int
		line  int
	}{{1, 12}, {2, 40}} {
		if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, observed.round, &StepOutcome{
			Findings:        mustMarshalFindings(types.Findings{Items: []types.Finding{moved(observed.line)}}),
			ReviewablePaths: []string{"lib.go"},
			ReviewedPaths:   []string{"lib.go"},
		}, "head-1"); err != nil {
			t.Fatalf("round %d: %v", observed.round, err)
		}
	}
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want one identity across the line shift", len(entries))
	}
	if entries[0].Line != 12 || entries[0].LastObservedLine != 40 {
		t.Fatalf("observations not both retained: line=%d last=%d", entries[0].Line, entries[0].LastObservedLine)
	}
	if entries[0].FirstSeenRound != 1 || entries[0].LastObservedRound != 2 {
		t.Fatalf("rounds not retained: first=%d last=%d", entries[0].FirstSeenRound, entries[0].LastObservedRound)
	}
}

// TestFindingLedger_ColdCertifierIsIndependentEvenWithAMatchingSessionID pins
// the distinction between resuming a session and merely reporting one: a cold
// turn is independent by construction, so a certifier that did not continue a
// session may certify the fix even when its adapter reports the fixing
// session's identity value.
func TestFindingLedger_ColdCertifierIsIndependentEvenWithAMatchingSessionID(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 1, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "f-1", Severity: "error", Action: types.ActionAutoFix, File: "a.go", Line: 2, Description: "leak"},
		}}),
		ReviewablePaths: []string{"a.go"},
		ReviewedPaths:   []string{"a.go"},
	}, "h1"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepReview, sr.ID, 1, []string{"f-1"}, "user"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if err := ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "h2", "same-looking-session"); err != nil {
		t.Fatalf("record correction: %v", err)
	}
	// Same reported identity, but the turn did not resume anything.
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr.ID, 2, &StepOutcome{
		Findings:            mustMarshalFindings(types.Findings{}),
		ReviewablePaths:     []string{"a.go"},
		ReviewedPaths:       []string{"a.go"},
		AgentSessionID:      "same-looking-session",
		AgentSessionResumed: false,
	}, "h2"); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if entries[0].Status != types.FindingLedgerStatusClosedVerified {
		t.Fatalf("status = %q, want closed_verified from a cold certifier", entries[0].Status)
	}

	// And the same identity WITH resumption is refused.
	sr2, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatalf("insert second step result: %v", err)
	}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr2.ID, 1, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{Items: []types.Finding{
			{ID: "f-2", Severity: "error", Action: types.ActionAutoFix, File: "b.go", Line: 2, Description: "leak b"},
		}}),
		ReviewablePaths: []string{"b.go"},
		ReviewedPaths:   []string{"b.go"},
	}, "h3"); err != nil {
		t.Fatalf("second round 1: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepReview, sr2.ID, 1, []string{"f-2"}, "user"); err != nil {
		t.Fatalf("second selection: %v", err)
	}
	if err := ledger.RecordCorrectingRevision(ctx, types.StepReview, 2, "h4", "resumed-session"); err != nil {
		t.Fatalf("second record correction: %v", err)
	}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepReview, sr2.ID, 2, &StepOutcome{
		Findings:            mustMarshalFindings(types.Findings{}),
		ReviewablePaths:     []string{"b.go"},
		ReviewedPaths:       []string{"b.go"},
		AgentSessionID:      "resumed-session",
		AgentSessionResumed: true,
	}, "h4"); err != nil {
		t.Fatalf("second round 2: %v", err)
	}
	entries, err = database.GetFindingLedgerEntriesByStep(runID, types.StepReview)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	for _, e := range entries {
		if e.ReportedID == "f-2" && e.Status == types.FindingLedgerStatusClosedVerified {
			t.Fatal("a resumed fixing session certified its own correction")
		}
	}
}

// TestFindingLedger_CISelectedRepairClosesVerifiedNotSuperseded pins the other
// half of CI closure: an entry that was actually selected and repaired is
// proven by the check's own re-run at the corrected revision, so it counts as a
// verified fix rather than a supersession.
func TestFindingLedger_CISelectedRepairClosesVerifiedNotSuperseded(t *testing.T) {
	database, runID, repoID := setupTestDB(t)
	ctx := context.Background()
	ledger := NewFindingLedger(database, runID, repoID)

	sr, err := database.InsertStepResult(runID, types.StepCI)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	check := types.Finding{ID: "ci-1", Severity: "error", Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck, Check: "test", CheckID: "gh:1", Description: "check failing"}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepCI, sr.ID, 1, &StepOutcome{
		NeedsApproval: true,
		Findings:      mustMarshalFindings(types.Findings{Items: []types.Finding{check}}),
		ExitCode:      1,
	}, "head-1"); err != nil {
		t.Fatalf("failing round: %v", err)
	}
	if err := ledger.ProcessSelection(ctx, types.StepCI, sr.ID, 1, []string{"ci-1"}, "auto_fix"); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if err := ledger.RecordCorrectingRevision(ctx, types.StepCI, 2, "head-2", "fixer"); err != nil {
		t.Fatalf("record correction: %v", err)
	}
	if _, err := ledger.ProcessRoundFindings(ctx, types.StepCI, sr.ID, 2, &StepOutcome{
		Findings: mustMarshalFindings(types.Findings{}),
		ExitCode: 0,
	}, "head-2"); err != nil {
		t.Fatalf("clean round: %v", err)
	}
	entries, err := database.GetFindingLedgerEntriesByStep(runID, types.StepCI)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != types.FindingLedgerStatusClosedVerified {
		t.Fatalf("entries = %+v, want one closed_verified check finding", entries)
	}
	if entries[0].CorrectingCommitSHA != "head-2" {
		t.Fatalf("correcting revision = %q, want head-2", entries[0].CorrectingCommitSHA)
	}
	stats, err := database.StepFindingStats(sr)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.FixedFindings != 1 || stats.ReportedFindings != 1 {
		t.Fatalf("stats = %+v, want the repaired check counted as fixed", stats)
	}
}
