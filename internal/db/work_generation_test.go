package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestWorkGeneration_InsertAndRetrieve(t *testing.T) {
	database := openTestDB(t)

	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	gen0 := &types.WorkGeneration{
		ID:                  "gen-0",
		RunID:               run.ID,
		RepoID:              repo.ID,
		Ordinal:             0,
		Cause:               types.GenerationCauseInitial,
		GenerationDigest:    "gen-hash-0",
		PlanID:              "plan-v1",
		PlanDigest:          "plan-hash-1",
		GitHeadSHA:          "head-1",
		GitTreeSHA:          "tree-1",
		InputManifestDigest: "manifest-hash-1",
		Phase:               types.StepReview,
		Status:              types.GenerationStatusActive,
	}

	if err := database.InsertWorkGeneration(gen0); err != nil {
		t.Fatalf("InsertWorkGeneration: %v", err)
	}

	retrieved, err := database.GetWorkGeneration(run.ID, "gen-0")
	if err != nil {
		t.Fatalf("GetWorkGeneration: %v", err)
	}
	if retrieved == nil || retrieved.GenerationDigest != "gen-hash-0" {
		t.Fatalf("unexpected retrieved generation: %+v", retrieved)
	}

	current, err := database.GetCurrentWorkGeneration(run.ID)
	if err != nil {
		t.Fatalf("GetCurrentWorkGeneration: %v", err)
	}
	if current == nil || current.ID != "gen-0" {
		t.Fatalf("expected current generation gen-0, got %+v", current)
	}

	// Insert generation 1
	gen1 := &types.WorkGeneration{
		ID:                     "gen-1",
		RunID:                  run.ID,
		RepoID:                 repo.ID,
		Ordinal:                1,
		ParentGenerationID:     "gen-0",
		ParentGenerationDigest: "gen-hash-0",
		Cause:                  types.GenerationCauseSourceMutation,
		GenerationDigest:       "gen-hash-1",
		PlanID:                 "plan-v1",
		PlanDigest:             "plan-hash-1",
		GitHeadSHA:             "head-2",
		GitTreeSHA:             "tree-2",
		InputManifestDigest:    "manifest-hash-2",
		Phase:                  types.StepReview,
		Status:                 types.GenerationStatusActive,
	}
	if err := database.InsertWorkGeneration(gen1); err != nil {
		t.Fatalf("InsertWorkGeneration gen1: %v", err)
	}

	current, err = database.GetCurrentWorkGeneration(run.ID)
	if err != nil {
		t.Fatalf("GetCurrentWorkGeneration: %v", err)
	}
	if current == nil || current.ID != "gen-1" {
		t.Fatalf("expected current generation gen-1, got %+v", current)
	}

	gens, err := database.GetWorkGenerationsByRun(run.ID)
	if err != nil {
		t.Fatalf("GetWorkGenerationsByRun: %v", err)
	}
	if len(gens) != 2 || gens[0].ID != "gen-0" || gens[1].ID != "gen-1" {
		t.Fatalf("expected 2 generations ordered by ordinal ASC, got %+v", gens)
	}
}

func TestWorkPhaseResult_InsertAndInvalidate(t *testing.T) {
	database := openTestDB(t)

	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	gen := &types.WorkGeneration{
		ID:                  "gen-0",
		RunID:               run.ID,
		RepoID:              repo.ID,
		Ordinal:             0,
		Cause:               types.GenerationCauseInitial,
		GenerationDigest:    "gen-hash-0",
		PlanID:              "plan-v1",
		PlanDigest:          "plan-hash-1",
		GitHeadSHA:          "head-1",
		GitTreeSHA:          "tree-1",
		InputManifestDigest: "manifest-hash-1",
		Phase:               types.StepTest,
		Status:              types.GenerationStatusActive,
	}
	if err := database.InsertWorkGeneration(gen); err != nil {
		t.Fatalf("InsertWorkGeneration: %v", err)
	}

	res := &types.WorkPhaseResult{
		ID:           "res-test-1",
		RunID:        run.ID,
		GenerationID: gen.ID,
		PlanID:       gen.PlanID,
		Phase:        types.StepTest,
		Status:       types.PhaseResultStatusPassed,
		Applicable:   true,
		EvidenceID:   "ev-test-1",
		OutputDigest: "out-digest-1",
	}
	if err := database.InsertWorkPhaseResult(res); err != nil {
		t.Fatalf("InsertWorkPhaseResult: %v", err)
	}

	results, err := database.GetWorkPhaseResultsByGeneration(run.ID, gen.ID)
	if err != nil {
		t.Fatalf("GetWorkPhaseResultsByGeneration: %v", err)
	}
	if len(results) != 1 || results[0].Status != types.PhaseResultStatusPassed || !results[0].Applicable {
		t.Fatalf("unexpected phase results: %+v", results)
	}

	// Invalidate Test result
	if err := database.InvalidateWorkPhaseResults(run.ID, gen.ID, []types.StepName{types.StepTest}, "source mutated"); err != nil {
		t.Fatalf("InvalidateWorkPhaseResults: %v", err)
	}

	results, err = database.GetWorkPhaseResultsByGeneration(run.ID, gen.ID)
	if err != nil {
		t.Fatalf("GetWorkPhaseResultsByGeneration after invalidation: %v", err)
	}
	if len(results) != 1 || results[0].Applicable || results[0].Status != types.PhaseResultStatusStale {
		t.Fatalf("expected stale invalidated result, got %+v", results[0])
	}
}

func TestWorkAttestation_InsertAndRetrieve(t *testing.T) {
	database := openTestDB(t)

	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	gen := &types.WorkGeneration{
		ID:                  "gen-0",
		RunID:               run.ID,
		RepoID:              repo.ID,
		Ordinal:             0,
		Cause:               types.GenerationCauseInitial,
		GenerationDigest:    "gen-hash-0",
		PlanID:              "plan-v1",
		PlanDigest:          "plan-hash-1",
		GitHeadSHA:          "head-1",
		GitTreeSHA:          "tree-1",
		InputManifestDigest: "manifest-hash-1",
		Phase:               types.StepCI,
		Status:              types.GenerationStatusSealed,
	}
	if err := database.InsertWorkGeneration(gen); err != nil {
		t.Fatalf("InsertWorkGeneration: %v", err)
	}

	att := &types.WorkAttestation{
		ID:                  "att-1",
		ProtocolVersion:     types.WorkGenerationProtocolVersion,
		RunID:               run.ID,
		GenerationID:        gen.ID,
		GenerationDigest:    gen.GenerationDigest,
		GenerationOrdinal:   0,
		PlanID:              gen.PlanID,
		PlanDigest:          gen.PlanDigest,
		FinalHeadSHA:        "head-1",
		FinalTreeSHA:        "tree-1",
		FinalEnvelopeDigest: "env-digest-1",
		PhaseResults: []types.WorkPhaseResultSummary{
			{ID: "res-1", Phase: types.StepReview, Status: types.PhaseResultStatusPassed, Applicable: true},
			{ID: "res-2", Phase: types.StepTest, Status: types.PhaseResultStatusPassed, Applicable: true},
		},
		CIHeadSHA:         "head-1",
		CICheckIdentity:   "gh-actions-run-123",
		AttestationDigest: "att-digest-1",
	}

	if err := database.InsertWorkAttestation(att); err != nil {
		t.Fatalf("InsertWorkAttestation: %v", err)
	}

	retrieved, err := database.GetWorkAttestation(run.ID)
	if err != nil {
		t.Fatalf("GetWorkAttestation: %v", err)
	}
	if retrieved == nil || retrieved.AttestationDigest != "att-digest-1" || len(retrieved.PhaseResults) != 2 {
		t.Fatalf("unexpected attestation: %+v", retrieved)
	}

	summary, err := database.GetWorkGenerationSummary(run.ID)
	if err != nil {
		t.Fatalf("GetWorkGenerationSummary: %v", err)
	}
	if summary == nil || summary.AttestationID != "att-1" || summary.CurrentGenerationID != "gen-0" {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestWorkGeneration_LegacyRunMigration(t *testing.T) {
	database := openTestDB(t)

	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}

	// 1. Completed run: migration must NOT modify it or insert migration marker
	completedRun, err := database.InsertRun(repo.ID, "feature", "head-done", "base-done")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := database.UpdateRunStatus(completedRun.ID, types.RunCompleted); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	if err := database.MigrateLegacyWorkGenerationForRun(completedRun.ID); err != nil {
		t.Fatalf("MigrateLegacyWorkGenerationForRun completed run: %v", err)
	}
	var count int
	if err := database.sql.QueryRow(`SELECT COUNT(*) FROM work_generation_migrations WHERE run_id = ?`, completedRun.ID).Scan(&count); err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	if count != 0 {
		t.Fatal("expected completed historical run to have no migration record")
	}

	// 2. Active run: migration records marker and invalidates active results
	activeRun, err := database.InsertRun(repo.ID, "feature-active", "head-active", "base-active")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := database.UpdateRunStatus(activeRun.ID, types.RunRunning); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	if err := database.MigrateLegacyWorkGenerationForRun(activeRun.ID); err != nil {
		t.Fatalf("MigrateLegacyWorkGenerationForRun active run: %v", err)
	}

	if err := database.sql.QueryRow(`SELECT COUNT(*) FROM work_generation_migrations WHERE run_id = ?`, activeRun.ID).Scan(&count); err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	if count != 1 {
		t.Fatal("expected active run to have 1 migration record")
	}

	// Idempotency: second migration call is a no-op
	if err := database.MigrateLegacyWorkGenerationForRun(activeRun.ID); err != nil {
		t.Fatalf("MigrateLegacyWorkGenerationForRun idempotent second call: %v", err)
	}
}
