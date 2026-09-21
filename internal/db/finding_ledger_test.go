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

	// Import this run the way the pipeline does: lazily, when a ledger is
	// created for it. Nothing is imported at Open, so a run the pipeline never
	// adopts keeps its pre-ledger accounting untouched.
	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("MigrateLegacyFindingLedgerForRun: %v", err)
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
	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("repeat MigrateLegacyFindingLedgerForRun: %v", err)
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

// TestFindingLedger_CompletedRunIsNeverImported pins the historical boundary:
// completed runs keep exactly the evidence they recorded. Importing them would
// have to invent a disposition (nobody accepted anything at import time) or,
// by switching StepFindingStats onto the ledger, silently restate old fixed
// counts as unfixed.
func TestFindingLedger_CompletedRunIsNeverImported(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := db.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	sr, err := db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("InsertStepResult: %v", err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":5,"description":"bug A","action":"auto-fix"}],"summary":"1 issue"}`
	if _, err := db.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("InsertStepRound: %v", err)
	}

	// Even an explicit request refuses a terminal run.
	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("MigrateLegacyFindingLedgerForRun: %v", err)
	}
	entries, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("completed run imported %d ledger entries, want 0", len(entries))
	}
	for _, e := range entries {
		if e.Status != types.FindingLedgerStatusOpen {
			t.Fatalf("imported entry %s has status %q, want open", e.ID, e.Status)
		}
	}
}

// TestFindingLedger_MigrationMarkerSurvivesAPartialAttempt pins the idempotency
// owner: the migration marker, not the presence of imported entries. A crash
// that left entries behind without the marker must be completed on retry rather
// than skipped as already-migrated.
func TestFindingLedger_MigrationMarkerSurvivesAPartialAttempt(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
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
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":5,"description":"bug A","action":"auto-fix"},{"id":"review-2","severity":"error","file":"b.go","line":9,"description":"bug B","action":"auto-fix"}],"summary":"2 issues"}`
	if _, err := db.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("InsertStepRound: %v", err)
	}

	// Seed entries without a marker. The transactional import cannot produce
	// this state, so this is a deliberate pin on the idempotency owner: if a
	// future change drops the transaction, "some entry exists" must still not
	// be allowed to stand in for the marker and skip the rest of a run.
	for _, id := range []string{"fn-partial-1"} {
		entry := &types.FindingLedgerEntry{
			ID: id, RunID: run.ID, RepoID: repo.ID, StepName: types.StepReview,
			FirstSeenRound: 1, FirstSeenStepResultID: sr.ID, ReportedID: "review-1",
			Fingerprint: "partial", Severity: "error", Action: types.ActionAutoFix,
			File: "a.go", Line: 5, Description: "bug A",
			OriginalFindingJSON: `{}`, CurrentFindingJSON: `{}`,
			Status: types.FindingLedgerStatusOpen, IsBlocking: true,
			LastObservedRound: 1, LastObservedFile: "a.go", LastObservedLine: 5,
		}
		if err := db.InsertFindingLedgerEntry(entry); err != nil {
			t.Fatalf("seed partial entry: %v", err)
		}
	}

	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("retry migration: %v", err)
	}

	// The retry completed the import: both real findings are present, and a
	// second retry is a no-op because the marker now exists.
	entries, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerEntries: %v", err)
	}
	reported := map[string]bool{}
	for _, e := range entries {
		reported[e.ReportedID] = true
	}
	if !reported["review-1"] || !reported["review-2"] {
		t.Fatalf("retry did not complete the import: %v", reported)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want the seeded partial plus both real findings", len(entries))
	}
	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	repeat, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerEntries: %v", err)
	}
	if len(repeat) != len(entries) {
		t.Fatalf("repeat migration changed entry count from %d to %d", len(entries), len(repeat))
	}
}

// TestFindingLedger_MigrationSkipsStepOwnedFindings pins that a pre-ledger run
// does not import an operator park its step re-derives from live state, because
// nothing in the ledger could ever close it.
func TestFindingLedger_MigrationSkipsStepOwnedFindings(t *testing.T) {
	db := openTestDB(t)

	repo, err := db.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := db.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	sr, err := db.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatalf("InsertStepResult: %v", err)
	}
	findings := `{"findings":[{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"budget cut"},{"id":"test-1","severity":"error","action":"auto-fix","file":"a.go","description":"scenario failed"}],"summary":"2 issues"}`
	if _, err := db.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("InsertStepRound: %v", err)
	}

	if err := db.MigrateLegacyFindingLedgerForRun(run.ID); err != nil {
		t.Fatalf("MigrateLegacyFindingLedgerForRun: %v", err)
	}
	entries, err := db.GetFindingLedgerEntries(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].ReportedID != "test-1" {
		t.Fatalf("imported entries = %+v, want only the analyzer finding test-1", entries)
	}
}

// TestFindingLedger_SummaryPublishesOnlyUnresolvedEntries pins the protocol's
// bounded publishable surface: counts cover every entry, Entries covers what
// still requires a disposition.
func TestFindingLedger_SummaryPublishesOnlyUnresolvedEntries(t *testing.T) {
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
	base := func(id, status string) *types.FindingLedgerEntry {
		return &types.FindingLedgerEntry{
			ID: id, RunID: run.ID, RepoID: repo.ID, StepName: types.StepReview,
			FirstSeenRound: 1, FirstSeenStepResultID: sr.ID, ReportedID: id,
			Fingerprint: id, Severity: "error", Action: types.ActionAutoFix,
			File: "a.go", Line: 1, Description: id,
			OriginalFindingJSON: `{}`, CurrentFindingJSON: `{}`,
			Status: status, IsBlocking: true,
			LastObservedRound: 1, LastObservedFile: "a.go", LastObservedLine: 1,
		}
	}
	for _, e := range []*types.FindingLedgerEntry{
		base("fn-open", types.FindingLedgerStatusOpen),
		base("fn-pending", types.FindingLedgerStatusPendingVerification),
		base("fn-reconcile", types.FindingLedgerStatusNeedsReconciliation),
		base("fn-verified", types.FindingLedgerStatusClosedVerified),
		base("fn-accepted", types.FindingLedgerStatusClosedAccepted),
	} {
		if err := db.InsertFindingLedgerEntry(e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	summary, err := db.GetFindingLedgerSummary(run.ID)
	if err != nil {
		t.Fatalf("GetFindingLedgerSummary: %v", err)
	}
	if summary.ProtocolVersion != types.FindingLedgerProtocolVersion {
		t.Fatalf("protocol_version = %q, want %q", summary.ProtocolVersion, types.FindingLedgerProtocolVersion)
	}
	if summary.TotalEntries != 5 || summary.OpenCount != 1 || summary.PendingCount != 1 || summary.ReconcileCount != 1 || summary.ClosedCount != 2 {
		t.Fatalf("summary counts = %+v", summary)
	}
	if !summary.HasBlocking || summary.Unresolved() != 3 {
		t.Fatalf("summary blocking=%v unresolved=%d, want true/3", summary.HasBlocking, summary.Unresolved())
	}
	if len(summary.Entries) != 3 {
		t.Fatalf("summary published %d entries, want only the 3 unresolved", len(summary.Entries))
	}
	for _, e := range summary.Entries {
		if types.IsClosedLedgerStatus(e.Status) {
			t.Fatalf("summary published closed entry %s", e.ID)
		}
	}
}
