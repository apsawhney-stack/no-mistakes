package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func setupWorkGenTest(t *testing.T) (*db.DB, *WorkGenerationManager, string, *db.Run) {
	t.Helper()
	dir := t.TempDir()
	p := paths.WithRoot(dir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	workDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(filepath.Join(workDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "src", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "docs", "readme.md"), []byte("# Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := database.InsertRepoWithID("testrepo", workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-sha-0", "base-sha-0")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ValidationPlan: types.ValidationPlan{
			PlanID:         "test-plan",
			SelectedInputs: []string{"src/**"},
			RequiredPhases: []types.StepName{types.StepReview, types.StepTest},
			PhaseWriteSets: map[string][]string{
				"review":   {"src/**"},
				"test":     {},
				"document": {"docs/**", "*.md"},
			},
			ProtectedExclusions: []string{".git/**", "protected/**"},
		},
	}
	cfg.ValidationPlan.PlanDigest = types.ComputePlanDigest(&cfg.ValidationPlan)

	mgr, err := NewWorkGenerationManager(database, run.ID, repo.ID, workDir, cfg)
	if err != nil {
		t.Fatalf("NewWorkGenerationManager: %v", err)
	}

	return database, mgr, workDir, run
}

func TestWorkGeneration_InitialGenerationCreation(t *testing.T) {
	ctx := context.Background()
	_, mgr, _, _ := setupWorkGenTest(t)

	gen, err := mgr.EnsureGeneration(ctx, "head-sha-0")
	if err != nil {
		t.Fatalf("EnsureGeneration: %v", err)
	}
	if gen.Ordinal != 0 {
		t.Errorf("ordinal = %d, want 0", gen.Ordinal)
	}
	if gen.Status != types.GenerationStatusActive {
		t.Errorf("status = %s, want active", gen.Status)
	}
	if gen.GenerationDigest == "" {
		t.Error("GenerationDigest is empty")
	}
	if gen.InputManifest == nil || len(gen.InputManifest.Entries) == 0 {
		t.Fatal("InputManifest entries empty")
	}
	// Verify that docs/readme.md was NOT included in selected inputs (src/**)
	for _, entry := range gen.InputManifest.Entries {
		if strings.HasPrefix(entry.Path, "docs/") {
			t.Errorf("found unselected doc path in manifest: %s", entry.Path)
		}
	}
}

func TestWorkGeneration_SelectedInputMutationIncrementsGeneration(t *testing.T) {
	ctx := context.Background()
	dbInstance, mgr, workDir, run := setupWorkGenTest(t)

	gen0, err := mgr.EnsureGeneration(ctx, "head-sha-0")
	if err != nil {
		t.Fatalf("EnsureGeneration: %v", err)
	}

	// Record a passing test result for generation 0
	res0, err := mgr.RecordPhaseResult(ctx, types.StepTest, types.PhaseResultStatusPassed, "test-cmd", "ev-1", "digest-1", nil)
	if err != nil {
		t.Fatalf("RecordPhaseResult: %v", err)
	}
	if !res0.Applicable {
		t.Error("res0 should be applicable")
	}

	// Mutate selected input src/main.go
	if err := os.WriteFile(filepath.Join(workDir, "src", "main.go"), []byte("package main\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gen1, err := mgr.HandleMutation(ctx, "fix_applied", types.StepReview, "head-sha-1", []string{"src/main.go"})
	if err != nil {
		t.Fatalf("HandleMutation: %v", err)
	}

	if gen1.Ordinal != 1 {
		t.Errorf("ordinal = %d, want 1", gen1.Ordinal)
	}
	if gen1.ParentGenerationID != gen0.ID {
		t.Errorf("ParentGenerationID = %s, want %s", gen1.ParentGenerationID, gen0.ID)
	}
	if gen1.GenerationDigest == gen0.GenerationDigest {
		t.Error("GenerationDigest did not change on mutation")
	}

	// Check that previous phase result res0 was invalidated
	results, err := dbInstance.GetWorkPhaseResultsByGeneration(run.ID, gen0.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Applicable {
		t.Errorf("expected res0 to be invalidated, got applicable=%v", results[0].Applicable)
	}
}

func TestWorkGeneration_NarrativeOnlyDocChangesPreserveGeneration(t *testing.T) {
	ctx := context.Background()
	_, mgr, workDir, _ := setupWorkGenTest(t)

	gen0, err := mgr.EnsureGeneration(ctx, "head-sha-0")
	if err != nil {
		t.Fatalf("EnsureGeneration: %v", err)
	}

	// Record passing review and test results
	if _, err := mgr.RecordPhaseResult(ctx, types.StepReview, types.PhaseResultStatusPassed, "rev", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.RecordPhaseResult(ctx, types.StepTest, types.PhaseResultStatusPassed, "tst", "ev-1", "d-1", nil); err != nil {
		t.Fatal(err)
	}

	// Mutate docs/readme.md (narrative only)
	if err := os.WriteFile(filepath.Join(workDir, "docs", "readme.md"), []byte("# Updated Docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sameGen, err := mgr.HandleMutation(ctx, "document_update", types.StepDocument, "head-sha-doc", []string{"docs/readme.md"})
	if err != nil {
		t.Fatalf("HandleMutation: %v", err)
	}

	if sameGen.Ordinal != 0 {
		t.Errorf("ordinal changed: got %d, want 0", sameGen.Ordinal)
	}
	if sameGen.ID != gen0.ID {
		t.Errorf("generation ID changed: got %s, want %s", sameGen.ID, gen0.ID)
	}
	if sameGen.GenerationDigest != gen0.GenerationDigest {
		t.Error("generation digest changed on narrative-only doc edit")
	}
}

func TestWorkGeneration_WriteSetEnforcement(t *testing.T) {
	ctx := context.Background()
	_, mgr, workDir, _ := setupWorkGenTest(t)

	if _, err := mgr.EnsureGeneration(ctx, "head-sha-0"); err != nil {
		t.Fatal(err)
	}

	// 1. Authorized write during review
	preReview, err := mgr.CheckPhasePreState(ctx, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "src", "helper.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictReview, err := mgr.CheckPhasePostState(ctx, types.StepReview, false, preReview)
	if err != nil {
		t.Fatal(err)
	}
	if !verdictReview.Allowed {
		t.Errorf("review write should be allowed, got: %s", verdictReview.Reason)
	}

	// 2. Unauthorized write during test (test write set is empty)
	preTest, err := mgr.CheckPhasePreState(ctx, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "src", "unauth.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictTest, err := mgr.CheckPhasePostState(ctx, types.StepTest, false, preTest)
	if err != nil {
		t.Fatal(err)
	}
	if verdictTest.Allowed {
		t.Error("test write should have been refused as unauthorized")
	}
	if len(verdictTest.UnauthorizedPaths) != 1 || verdictTest.UnauthorizedPaths[0] != "src/unauth.go" {
		t.Errorf("unexpected unauthorized paths: %v", verdictTest.UnauthorizedPaths)
	}

	// 3. Protected exclusion write during review (review is allowed src/**, but protected/** is excluded engine-wide)
	if err := os.MkdirAll(filepath.Join(workDir, "protected"), 0o755); err != nil {
		t.Fatal(err)
	}
	preProtected, err := mgr.CheckPhasePreState(ctx, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "protected", "secret.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictProtected, err := mgr.CheckPhasePostState(ctx, types.StepReview, false, preProtected)
	if err != nil {
		t.Fatal(err)
	}
	if verdictProtected.Allowed {
		t.Error("protected exclusion write should have been refused")
	}
}

func TestWorkGeneration_ReopensLedgerClosuresOnMutation(t *testing.T) {
	ctx := context.Background()
	dbInstance, mgr, workDir, run := setupWorkGenTest(t)

	gen0, err := mgr.EnsureGeneration(ctx, "head-sha-0")
	if err != nil {
		t.Fatal(err)
	}

	sr, err := dbInstance.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}

	// Add a closed_verified finding to the ledger
	entry := &types.FindingLedgerEntry{
		ID:                    "fn-test-1",
		RunID:                 run.ID,
		RepoID:                "testrepo",
		StepName:              types.StepReview,
		FirstSeenRound:        1,
		FirstSeenStepResultID: sr.ID,
		ReportedID:            "rev-1",
		Fingerprint:           "fp-test-1",
		Severity:              "error",
		Action:                "auto-fix",
		File:                  "src/main.go",
		Line:                  10,
		Description:           "defect in main.go",
		Status:                types.FindingLedgerStatusClosedVerified,
		ClosureEvidence:       "verified fix",
		ClosureReason:         "fixed in review",
		ClosureGenerationID:   gen0.ID,
		GenerationID:          gen0.ID,
		LastObservedRound:     1,
		LastObservedFile:      "src/main.go",
		CreatedAt:             100,
		UpdatedAt:             100,
	}
	if err := dbInstance.InsertFindingLedgerEntry(entry); err != nil {
		t.Fatal(err)
	}

	// Mutate src/main.go
	if err := os.WriteFile(filepath.Join(workDir, "src", "main.go"), []byte("package main\nfunc main() { panic(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gen1, err := mgr.HandleMutation(ctx, "new_code_added", types.StepReview, "head-sha-1", []string{"src/main.go"})
	if err != nil {
		t.Fatal(err)
	}

	// Verify that the finding was reopened to open
	reloaded, err := dbInstance.GetFindingLedgerEntry(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != types.FindingLedgerStatusOpen {
		t.Errorf("status = %s, want open", reloaded.Status)
	}
	if reloaded.ClosureEvidence != "" {
		t.Errorf("closure evidence not cleared: %s", reloaded.ClosureEvidence)
	}

	// Verify event was recorded
	events, err := dbInstance.GetFindingLedgerEvents(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundInvalidated := false
	for _, ev := range events {
		if ev.EventType == types.FindingEventGenerationInvalidated {
			foundInvalidated = true
			if ev.GenerationID != gen1.ID {
				t.Errorf("event GenerationID = %s, want %s", ev.GenerationID, gen1.ID)
			}
		}
	}
	if !foundInvalidated {
		t.Error("FindingEventGenerationInvalidated event not recorded")
	}
}

func TestWorkGeneration_AssertAcceptance(t *testing.T) {
	ctx := context.Background()
	_, mgr, _, run := setupWorkGenTest(t)

	if _, err := mgr.EnsureGeneration(ctx, "head-sha-0"); err != nil {
		t.Fatal(err)
	}

	// Refusal: missing required phase results (review and test)
	_, err := mgr.AssertAcceptance(ctx, "head-sha-0", true)
	if err == nil || !strings.Contains(err.Error(), "missing required phase results") {
		t.Fatalf("expected missing required phase results refusal, got: %v", err)
	}

	// Record review passed
	if _, err := mgr.RecordPhaseResult(ctx, types.StepReview, types.PhaseResultStatusPassed, "rev", "", "", nil); err != nil {
		t.Fatal(err)
	}

	// Still missing test
	_, err = mgr.AssertAcceptance(ctx, "head-sha-0", true)
	if err == nil || !strings.Contains(err.Error(), "missing required phase results: test") {
		t.Fatalf("expected missing test result refusal, got: %v", err)
	}

	// Record test passed
	if _, err := mgr.RecordPhaseResult(ctx, types.StepTest, types.PhaseResultStatusPassed, "test-cmd", "ev-dir", "out-digest", nil); err != nil {
		t.Fatal(err)
	}

	// Acceptance should now pass!
	att, err := mgr.AssertAcceptance(ctx, "head-sha-0", true)
	if err != nil {
		t.Fatalf("AssertAcceptance failed: %v", err)
	}
	if att.RunID != run.ID {
		t.Errorf("att.RunID = %s, want %s", att.RunID, run.ID)
	}
	if att.AttestationDigest == "" {
		t.Error("att.AttestationDigest is empty")
	}
	if att.FinalEnvelopeDigest == "" {
		t.Error("att.FinalEnvelopeDigest is empty")
	}
}
