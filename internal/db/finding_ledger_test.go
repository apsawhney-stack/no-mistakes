package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFindingLedger_CRUDAndEvents(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := db.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	sr, err := db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("InsertStepResult: %v", err)
	}

	entry := &types.FindingLedgerEntry{
		ID:                    "fn-test-01",
		RunID:                 run.ID,
		RepoID:                repo.ID,
		StepName:              types.StepReview,
		FirstSeenRound:        1,
		FirstSeenStepResultID: sr.ID,
		ReportedID:            "review-1",
		Fingerprint:           "file=main.go|desc=nil check|sev=error|cat=|chk=|cid=|did=|scope=",
		Severity:              "error",
		Action:                "auto-fix",
		File:                  "main.go",
		Line:                  10,
		Description:           "nil check",
		OriginalFindingJSON:   `{"id":"review-1","severity":"error","file":"main.go","line":10,"description":"nil check","action":"auto-fix"}`,
		CurrentFindingJSON:    `{"id":"review-1","severity":"error","file":"main.go","line":10,"description":"nil check","action":"auto-fix"}`,
		Status:                types.FindingLedgerStatusOpen,
		IsBlocking:            true,
		LastObservedRound:     1,
		LastObservedFile:      "main.go",
		LastObservedLine:      10,
	}

	if err := db.InsertFindingLedgerEntry(entry); err != nil {
		t.Fatalf("InsertFindingLedgerEntry: %v", err)
	}

	got, err := db.GetFindingLedgerEntry("fn-test-01")
	if err != nil || got == nil {
		t.Fatalf("GetFindingLedgerEntry: %v, %v", got, err)
	}
	if got.Status != types.FindingLedgerStatusOpen || !got.IsBlocking || got.ReportedID != "review-1" {
		t.Fatalf("unexpected entry fields: %+v", got)
	}

	// Update to pending_verification
	got.Status = types.FindingLedgerStatusPendingVerification
	got.SelectedInRound = 1
	got.CorrectingCommitSHA = "commit-abc"
	if err := db.UpdateFindingLedgerEntry(got); err != nil {
		t.Fatalf("UpdateFindingLedgerEntry: %v", err)
	}

	ev := &types.FindingLedgerEvent{
		EntryID:     got.ID,
		RunID:       run.ID,
		StepName:    types.StepReview,
		Round:       1,
		EventType:   types.FindingEventSelectedForCorrection,
		StateBefore: types.FindingLedgerStatusOpen,
		StateAfter:  types.FindingLedgerStatusPendingVerification,
		CommitSHA:   "commit-abc",
	}
	if err := db.RecordFindingLedgerEvent(ev); err != nil {
		t.Fatalf("RecordFindingLedgerEvent: %v", err)
	}

	events, err := db.GetFindingLedgerEvents(got.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("GetFindingLedgerEvents: %v, len=%d", err, len(events))
	}
	if events[0].EventType != types.FindingEventSelectedForCorrection || events[0].CommitSHA != "commit-abc" {
		t.Fatalf("unexpected event: %+v", events[0])
	}

	// Summary check
	summary, err := db.GetFindingLedgerSummary(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerSummary: %v", err)
	}
	if summary.TotalEntries != 1 || summary.PendingCount != 1 || !summary.HasBlocking {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestFindingLedger_LegacyMigration_ActiveRunOmissionRequiresReconciliation(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	// Active run
	run, err := db.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	sr, err := db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("InsertStepResult: %v", err)
	}

	// Round 1 produced F1 (file A) and F2 (file B)
	r1Findings := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":5,"description":"bug A","action":"auto-fix"},{"id":"review-2","severity":"error","file":"b.go","line":12,"description":"bug B","action":"auto-fix"}],"summary":"2 issues"}`
	_, err = db.InsertStepRound(sr.ID, 1, "initial", &r1Findings, nil, 100)
	if err != nil {
		t.Fatalf("InsertStepRound 1: %v", err)
	}

	// Round 2 only reported F2 (file B); F1 was omitted without verification proof!
	r2Findings := `{"findings":[{"id":"review-2","severity":"error","file":"b.go","line":12,"description":"bug B","action":"auto-fix"}],"summary":"1 issue"}`
	_, err = db.InsertStepRound(sr.ID, 2, "auto_fix", &r2Findings, nil, 100)
	if err != nil {
		t.Fatalf("InsertStepRound 2: %v", err)
	}

	// Clean out any entries if auto-migrated during insert to simulate legacy pre-ledger DB.
	_, _ = db.sql.Exec(`DELETE FROM finding_ledger_entries WHERE run_id = ?`, run.ID)
	_, _ = db.sql.Exec(`DELETE FROM finding_ledger_events WHERE run_id = ?`, run.ID)

	// Run migration
	if err := db.migrateLegacyFindingLedger(); err != nil {
		t.Fatalf("migrateLegacyFindingLedger: %v", err)
	}

	entries, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}

	var f1, f2 *types.FindingLedgerEntry
	for _, e := range entries {
		if e.File == "a.go" {
			f1 = e
		} else if e.File == "b.go" {
			f2 = e
		}
	}
	if f1 == nil || f2 == nil {
		t.Fatalf("missing f1 or f2: f1=%v, f2=%v", f1, f2)
	}

	// F1 was omitted in round 2 in an active run -> must be needs_reconciliation!
	if f1.Status != types.FindingLedgerStatusNeedsReconciliation {
		t.Errorf("f1 status = %q, want %q", f1.Status, types.FindingLedgerStatusNeedsReconciliation)
	}
	// F2 was still in round 2 -> open
	if f2.Status != types.FindingLedgerStatusOpen {
		t.Errorf("f2 status = %q, want %q", f2.Status, types.FindingLedgerStatusOpen)
	}

	// Idempotency check: running migration again does not duplicate or alter entries
	if err := db.migrateLegacyFindingLedger(); err != nil {
		t.Fatalf("repeat migrateLegacyFindingLedger: %v", err)
	}
	repeatEntries, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil || len(repeatEntries) != 2 {
		t.Fatalf("repeat migration changed entries: %v, len=%d", err, len(repeatEntries))
	}
}

func TestFindingLedger_StatsDistinguishesVerifiedFixFromAcceptance(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := db.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	sr, err := db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("InsertStepResult: %v", err)
	}

	// Finding 1: closed_verified
	e1 := &types.FindingLedgerEntry{
		ID:                    "fn-v1",
		RunID:                 run.ID,
		RepoID:                repo.ID,
		StepName:              types.StepReview,
		FirstSeenRound:        1,
		FirstSeenStepResultID: sr.ID,
		ReportedID:            "review-1",
		Fingerprint:           "file=a.go|desc=bug1|sev=error|cat=|chk=|cid=|did=|scope=",
		Severity:              "error",
		Action:                "auto-fix",
		File:                  "a.go",
		Line:                  1,
		Description:           "bug1",
		Status:                types.FindingLedgerStatusClosedVerified,
		IsBlocking:            true,
		OriginalFindingJSON:   `{"id":"review-1"}`,
		CurrentFindingJSON:    `{"id":"review-1"}`,
	}
	if err := db.InsertFindingLedgerEntry(e1); err != nil {
		t.Fatalf("Insert e1: %v", err)
	}

	// Finding 2: closed_accepted (operator override/waiver - NOT fixed!)
	e2 := &types.FindingLedgerEntry{
		ID:                    "fn-v2",
		RunID:                 run.ID,
		RepoID:                repo.ID,
		StepName:              types.StepReview,
		FirstSeenRound:        1,
		FirstSeenStepResultID: sr.ID,
		ReportedID:            "review-2",
		Fingerprint:           "file=b.go|desc=bug2|sev=warning|cat=|chk=|cid=|did=|scope=",
		Severity:              "warning",
		Action:                "ask-user",
		File:                  "b.go",
		Line:                  2,
		Description:           "bug2",
		Status:                types.FindingLedgerStatusClosedAccepted,
		IsBlocking:            true,
		OriginalFindingJSON:   `{"id":"review-2"}`,
		CurrentFindingJSON:    `{"id":"review-2"}`,
	}
	if err := db.InsertFindingLedgerEntry(e2); err != nil {
		t.Fatalf("Insert e2: %v", err)
	}

	stats, err := db.StepFindingStats(sr)
	if err != nil {
		t.Fatalf("StepFindingStats: %v", err)
	}

	// Total reported mistakes = 2
	// Fixed mistakes = 1 (only closed_verified, closed_accepted is an exception, NOT a fix!)
	if stats.ReportedFindings != 2 {
		t.Errorf("ReportedFindings = %d, want 2", stats.ReportedFindings)
	}
	if stats.FixedFindings != 1 {
		t.Errorf("FixedFindings = %d, want 1 (acceptance must not turn into fixed)", stats.FixedFindings)
	}
}
