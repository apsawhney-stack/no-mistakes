package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const workGenSelectCols = `id, run_id, repo_id, ordinal, COALESCE(parent_generation_id, ''),
	cause, generation_digest, plan_id, plan_digest, plan_yaml, git_head_sha, git_tree_sha,
	input_manifest_json, input_manifest_digest, command_identities_json,
	config_digest, toolchain_digest, dependency_lock_digest, phase, status, created_at`

func scanWorkGeneration(scan func(...interface{}) error) (*types.WorkGeneration, error) {
	gen := &types.WorkGeneration{}
	var parentGenID string
	var planYAML string
	var manifestJSON string
	var cmdJSON string
	var phase string

	if err := scan(
		&gen.ID, &gen.RunID, &gen.RepoID, &gen.Ordinal, &parentGenID,
		&gen.Cause, &gen.GenerationDigest, &gen.PlanID, &gen.PlanDigest, &planYAML,
		&gen.GitHeadSHA, &gen.GitTreeSHA, &manifestJSON, &gen.InputManifestDigest,
		&cmdJSON, &gen.ConfigDigest, &gen.ToolchainDigest, &gen.DependencyLockDigest,
		&phase, &gen.Status, &gen.CreatedAt,
	); err != nil {
		return nil, err
	}

	gen.ParentGenerationID = parentGenID
	gen.PlanYAML = planYAML
	gen.Phase = types.StepName(phase)

	if manifestJSON != "" && manifestJSON != "{}" {
		var manifest types.InputManifest
		if err := json.Unmarshal([]byte(manifestJSON), &manifest); err == nil {
			gen.InputManifest = &manifest
		}
	}

	if cmdJSON != "" && cmdJSON != "{}" {
		var cmds map[string]string
		if err := json.Unmarshal([]byte(cmdJSON), &cmds); err == nil {
			gen.CommandIdentities = cmds
		}
	}

	return gen, nil
}

// InsertWorkGeneration inserts a new immutable work generation and updates the run's current generation pointer.
func (d *DB) InsertWorkGeneration(gen *types.WorkGeneration) error {
	if gen.ID == "" {
		gen.ID = "gen-" + newID()
	}
	if gen.CreatedAt == 0 {
		gen.CreatedAt = now()
	}
	if gen.Status == "" {
		gen.Status = types.GenerationStatusActive
	}

	manifestBytes, _ := json.Marshal(gen.InputManifest)
	if gen.InputManifest == nil {
		manifestBytes = []byte("{}")
	}
	cmdBytes, _ := json.Marshal(gen.CommandIdentities)
	if gen.CommandIdentities == nil {
		cmdBytes = []byte("{}")
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin insert work generation: %w", err)
	}
	defer tx.Rollback()

	var parentID *string
	if gen.ParentGenerationID != "" {
		parentID = &gen.ParentGenerationID
	}

	_, err = tx.Exec(
		`INSERT INTO work_generations (
			id, run_id, repo_id, ordinal, parent_generation_id,
			cause, generation_digest, plan_id, plan_digest, plan_yaml,
			git_head_sha, git_tree_sha, input_manifest_json, input_manifest_digest,
			command_identities_json, config_digest, toolchain_digest, dependency_lock_digest,
			phase, status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gen.ID, gen.RunID, gen.RepoID, gen.Ordinal, parentID,
		gen.Cause, gen.GenerationDigest, gen.PlanID, gen.PlanDigest, gen.PlanYAML,
		gen.GitHeadSHA, gen.GitTreeSHA, string(manifestBytes), gen.InputManifestDigest,
		string(cmdBytes), gen.ConfigDigest, gen.ToolchainDigest, gen.DependencyLockDigest,
		string(gen.Phase), gen.Status, gen.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert work generation row: %w", err)
	}

	// Update run current_generation_id and validation_plan_digest
	_, err = tx.Exec(
		`UPDATE runs SET current_generation_id = ?, validation_plan_digest = ?, updated_at = ? WHERE id = ?`,
		gen.ID, gen.PlanDigest, now(), gen.RunID,
	)
	if err != nil {
		return fmt.Errorf("update run current generation pointer: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit work generation insert: %w", err)
	}
	return nil
}

// GetWorkGeneration retrieves a specific work generation by ID.
func (d *DB) GetWorkGeneration(runID, genID string) (*types.WorkGeneration, error) {
	row := d.sql.QueryRow(
		`SELECT `+workGenSelectCols+` FROM work_generations WHERE run_id = ? AND id = ?`,
		runID, genID,
	)
	gen, err := scanWorkGeneration(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get work generation %s: %w", genID, err)
	}
	return gen, nil
}

// GetCurrentWorkGeneration returns the latest work generation for a run, ordered by ordinal DESC.
func (d *DB) GetCurrentWorkGeneration(runID string) (*types.WorkGeneration, error) {
	row := d.sql.QueryRow(
		`SELECT `+workGenSelectCols+` FROM work_generations WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1`,
		runID,
	)
	gen, err := scanWorkGeneration(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get current work generation for run %s: %w", runID, err)
	}
	return gen, nil
}

// GetWorkGenerationsByRun returns all work generations for a run, ordered by ordinal ASC.
func (d *DB) GetWorkGenerationsByRun(runID string) ([]*types.WorkGeneration, error) {
	rows, err := d.sql.Query(
		`SELECT `+workGenSelectCols+` FROM work_generations WHERE run_id = ? ORDER BY ordinal ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query work generations for run %s: %w", runID, err)
	}
	defer rows.Close()

	var result []*types.WorkGeneration
	for rows.Next() {
		gen, err := scanWorkGeneration(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan work generation row: %w", err)
		}
		result = append(result, gen)
	}
	return result, rows.Err()
}

// SealWorkGeneration marks a generation as sealed by a given phase.
func (d *DB) SealWorkGeneration(runID, genID string, phase types.StepName) error {
	_, err := d.sql.Exec(
		`UPDATE work_generations SET status = ?, phase = ? WHERE run_id = ? AND id = ?`,
		types.GenerationStatusSealed, string(phase), runID, genID,
	)
	if err != nil {
		return fmt.Errorf("seal work generation %s: %w", genID, err)
	}
	return nil
}

// SupersedeWorkGeneration marks a generation as superseded.
func (d *DB) SupersedeWorkGeneration(runID, genID string) error {
	_, err := d.sql.Exec(
		`UPDATE work_generations SET status = ? WHERE run_id = ? AND id = ?`,
		types.GenerationStatusSuperseded, runID, genID,
	)
	if err != nil {
		return fmt.Errorf("supersede work generation %s: %w", genID, err)
	}
	return nil
}

// InsertWorkPhaseResult inserts an immutable phase result bound to a generation.
func (d *DB) InsertWorkPhaseResult(res *types.WorkPhaseResult) error {
	if res.ID == "" {
		res.ID = "res-" + newID()
	}
	if res.CreatedAt == 0 {
		res.CreatedAt = now()
	}
	applicableInt := 0
	if res.Applicable {
		applicableInt = 1
	}

	depBytes, _ := json.Marshal(res.DependencyIdentities)
	if res.DependencyIdentities == nil {
		depBytes = []byte("[]")
	}

	_, err := d.sql.Exec(
		`INSERT INTO work_phase_results (
			id, run_id, generation_id, plan_id, phase, status, applicable,
			command_identity, dependency_identities_json, evidence_id, output_digest,
			invalidation_reason, created_at, invalidated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		res.ID, res.RunID, res.GenerationID, res.PlanID, string(res.Phase), res.Status, applicableInt,
		res.CommandIdentity, string(depBytes), res.EvidenceID, res.OutputDigest,
		res.InvalidationReason, res.CreatedAt, res.InvalidatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert work phase result: %w", err)
	}
	return nil
}

const workPhaseResultSelectCols = `id, run_id, generation_id, plan_id, phase, status, applicable,
	command_identity, dependency_identities_json, evidence_id, output_digest,
	invalidation_reason, created_at, invalidated_at`

func scanWorkPhaseResult(scan func(...interface{}) error) (*types.WorkPhaseResult, error) {
	res := &types.WorkPhaseResult{}
	var phase string
	var applicableInt int
	var depJSON string

	if err := scan(
		&res.ID, &res.RunID, &res.GenerationID, &res.PlanID, &phase, &res.Status, &applicableInt,
		&res.CommandIdentity, &depJSON, &res.EvidenceID, &res.OutputDigest,
		&res.InvalidationReason, &res.CreatedAt, &res.InvalidatedAt,
	); err != nil {
		return nil, err
	}

	res.Phase = types.StepName(phase)
	res.Applicable = applicableInt != 0

	if depJSON != "" && depJSON != "[]" {
		var deps []string
		if err := json.Unmarshal([]byte(depJSON), &deps); err == nil {
			res.DependencyIdentities = deps
		}
	}

	return res, nil
}

// GetWorkPhaseResultsByGeneration returns all phase results bound to a generation.
func (d *DB) GetWorkPhaseResultsByGeneration(runID, genID string) ([]*types.WorkPhaseResult, error) {
	rows, err := d.sql.Query(
		`SELECT `+workPhaseResultSelectCols+` FROM work_phase_results WHERE run_id = ? AND generation_id = ? ORDER BY created_at ASC`,
		runID, genID,
	)
	if err != nil {
		return nil, fmt.Errorf("query phase results for generation %s: %w", genID, err)
	}
	defer rows.Close()

	var result []*types.WorkPhaseResult
	for rows.Next() {
		res, err := scanWorkPhaseResult(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan work phase result: %w", err)
		}
		result = append(result, res)
	}
	return result, rows.Err()
}

// GetWorkPhaseResultsByRun returns all phase results for a run, ordered by created_at ASC.
func (d *DB) GetWorkPhaseResultsByRun(runID string) ([]*types.WorkPhaseResult, error) {
	rows, err := d.sql.Query(
		`SELECT `+workPhaseResultSelectCols+` FROM work_phase_results WHERE run_id = ? ORDER BY created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query phase results for run %s: %w", runID, err)
	}
	defer rows.Close()

	var result []*types.WorkPhaseResult
	for rows.Next() {
		res, err := scanWorkPhaseResult(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan work phase result: %w", err)
		}
		result = append(result, res)
	}
	return result, rows.Err()
}

// InvalidateWorkPhaseResults marks matching phase results as stale/invalidated.
func (d *DB) InvalidateWorkPhaseResults(runID, genID string, phases []types.StepName, reason string) error {
	if len(phases) == 0 {
		return nil
	}
	ts := now()
	for _, p := range phases {
		_, err := d.sql.Exec(
			`UPDATE work_phase_results SET applicable = 0, status = ?, invalidation_reason = ?, invalidated_at = ?
			 WHERE run_id = ? AND generation_id = ? AND phase = ? AND applicable = 1`,
			types.PhaseResultStatusStale, reason, ts, runID, genID, string(p),
		)
		if err != nil {
			return fmt.Errorf("invalidate phase result %s: %w", p, err)
		}
	}
	return nil
}

// InvalidateAllActivePhaseResults marks all currently applicable phase results for a run as stale.
func (d *DB) InvalidateAllActivePhaseResults(runID string, reason string) error {
	ts := now()
	_, err := d.sql.Exec(
		`UPDATE work_phase_results SET applicable = 0, status = ?, invalidation_reason = ?, invalidated_at = ?
		 WHERE run_id = ? AND applicable = 1`,
		types.PhaseResultStatusStale, reason, ts, runID,
	)
	if err != nil {
		return fmt.Errorf("invalidate active phase results for run %s: %w", runID, err)
	}
	return nil
}

// InsertWorkAttestation persists the final attestation binding for a run.
func (d *DB) InsertWorkAttestation(att *types.WorkAttestation) error {
	if att.ID == "" {
		att.ID = "att-" + newID()
	}
	if att.CreatedAt == 0 {
		att.CreatedAt = now()
	}
	if att.ProtocolVersion == "" {
		att.ProtocolVersion = types.WorkGenerationProtocolVersion
	}

	phaseResultsJSON, _ := json.Marshal(att.PhaseResults)
	evidenceJSON, _ := json.Marshal(att.EvidenceIdentities)
	ledgerJSON, _ := json.Marshal(att.LedgerSummary)
	invalidationJSON, _ := json.Marshal(att.InvalidationHistory)

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin insert work attestation: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO work_attestations (
			id, run_id, protocol_version, generation_id, generation_digest,
			generation_ordinal, plan_id, plan_digest, final_head_sha, final_tree_sha,
			final_envelope_digest, phase_results_json, evidence_identities_json,
			ledger_summary_json, invalidation_history_json, ci_head_sha, ci_check_identity,
			attestation_digest, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		att.ID, att.RunID, att.ProtocolVersion, att.GenerationID, att.GenerationDigest,
		att.GenerationOrdinal, att.PlanID, att.PlanDigest, att.FinalHeadSHA, att.FinalTreeSHA,
		att.FinalEnvelopeDigest, string(phaseResultsJSON), string(evidenceJSON),
		string(ledgerJSON), string(invalidationJSON), att.CIHeadSHA, att.CICheckIdentity,
		att.AttestationDigest, att.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert work attestation row: %w", err)
	}

	// Update run work_attestation_id
	_, err = tx.Exec(
		`UPDATE runs SET work_attestation_id = ?, updated_at = ? WHERE id = ?`,
		att.ID, now(), att.RunID,
	)
	if err != nil {
		return fmt.Errorf("update run work attestation pointer: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit work attestation insert: %w", err)
	}
	return nil
}

// GetWorkAttestation retrieves the published work attestation for a run.
func (d *DB) GetWorkAttestation(runID string) (*types.WorkAttestation, error) {
	row := d.sql.QueryRow(
		`SELECT id, run_id, protocol_version, generation_id, generation_digest,
			generation_ordinal, plan_id, plan_digest, final_head_sha, final_tree_sha,
			final_envelope_digest, phase_results_json, evidence_identities_json,
			ledger_summary_json, invalidation_history_json, ci_head_sha, ci_check_identity,
			attestation_digest, created_at
		 FROM work_attestations WHERE run_id = ?`,
		runID,
	)

	att := &types.WorkAttestation{}
	var phaseResultsJSON, evidenceJSON, ledgerJSON, invalidationJSON string

	err := row.Scan(
		&att.ID, &att.RunID, &att.ProtocolVersion, &att.GenerationID, &att.GenerationDigest,
		&att.GenerationOrdinal, &att.PlanID, &att.PlanDigest, &att.FinalHeadSHA, &att.FinalTreeSHA,
		&att.FinalEnvelopeDigest, &phaseResultsJSON, &evidenceJSON,
		&ledgerJSON, &invalidationJSON, &att.CIHeadSHA, &att.CICheckIdentity,
		&att.AttestationDigest, &att.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get work attestation for run %s: %w", runID, err)
	}

	if phaseResultsJSON != "" {
		_ = json.Unmarshal([]byte(phaseResultsJSON), &att.PhaseResults)
	}
	if evidenceJSON != "" {
		_ = json.Unmarshal([]byte(evidenceJSON), &att.EvidenceIdentities)
	}
	if ledgerJSON != "" {
		_ = json.Unmarshal([]byte(ledgerJSON), &att.LedgerSummary)
	}
	if invalidationJSON != "" {
		_ = json.Unmarshal([]byte(invalidationJSON), &att.InvalidationHistory)
	}

	return att, nil
}

// MigrateLegacyWorkGenerationForRun performs idempotent migration for active pre-F runs.
// Completed runs are left untouched.
// Active runs have their legacy evidence marked stale/unknown and are marked as migrated.
func (d *DB) MigrateLegacyWorkGenerationForRun(runID string) error {
	run, err := d.GetRun(runID)
	if err != nil {
		return fmt.Errorf("load run for work generation migration: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %s not found", runID)
	}

	// Completed historical runs are never imported or modified.
	if run.Status.Terminal() {
		return nil
	}

	// Check if already migrated
	var migrated int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM work_generation_migrations WHERE run_id = ?`, runID).Scan(&migrated); err != nil {
		return fmt.Errorf("check work generation migration: %w", err)
	}
	if migrated > 0 {
		return nil
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin work generation migration: %w", err)
	}
	defer tx.Rollback()

	// Invalidate any pre-existing active phase results as stale due to unknown generation provenance
	ts := now()
	_, err = tx.Exec(
		`UPDATE work_phase_results SET applicable = 0, status = ?, invalidation_reason = ?, invalidated_at = ?
		 WHERE run_id = ? AND applicable = 1`,
		types.PhaseResultStatusStale, "legacy active run migrated with unknown generation provenance", ts, runID,
	)
	if err != nil {
		return fmt.Errorf("invalidate legacy phase results: %w", err)
	}

	// Record migration marker
	_, err = tx.Exec(
		`INSERT INTO work_generation_migrations (run_id, protocol_version, imported_at) VALUES (?, ?, ?)`,
		runID, types.WorkGenerationProtocolVersion, ts,
	)
	if err != nil {
		return fmt.Errorf("record work generation migration marker: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit work generation migration: %w", err)
	}
	return nil
}

// GetWorkGenerationSummary builds a publishable summary for a run.
func (d *DB) GetWorkGenerationSummary(runID string) (*types.WorkGenerationSummary, error) {
	currentGen, err := d.GetCurrentWorkGeneration(runID)
	if err != nil {
		return nil, err
	}
	if currentGen == nil {
		return nil, nil
	}

	phaseResults, err := d.GetWorkPhaseResultsByGeneration(runID, currentGen.ID)
	if err != nil {
		return nil, err
	}

	var summaries []types.WorkPhaseResultSummary
	for _, pr := range phaseResults {
		summaries = append(summaries, types.WorkPhaseResultSummary{
			ID:                 pr.ID,
			Phase:              pr.Phase,
			Status:             pr.Status,
			Applicable:         pr.Applicable,
			EvidenceID:         pr.EvidenceID,
			OutputDigest:       pr.OutputDigest,
			InvalidationReason: pr.InvalidationReason,
		})
	}

	att, err := d.GetWorkAttestation(runID)
	if err != nil {
		return nil, err
	}

	sum := &types.WorkGenerationSummary{
		ProtocolVersion:          types.WorkGenerationProtocolVersion,
		RunID:                    runID,
		CurrentGenerationOrdinal: currentGen.Ordinal,
		CurrentGenerationID:      currentGen.ID,
		CurrentGenerationDigest:  currentGen.GenerationDigest,
		ParentGenerationID:       currentGen.ParentGenerationID,
		MutationCause:            currentGen.Cause,
		PlanID:                   currentGen.PlanID,
		PlanVersion:              "v1",
		PlanDigest:               currentGen.PlanDigest,
		WriteSetVerdict:          types.WriteSetVerdictClean,
		PhaseResults:             summaries,
	}

	if att != nil {
		sum.AttestationID = att.ID
		sum.AttestationDigest = att.AttestationDigest
		sum.FinalEnvelopeDigest = att.FinalEnvelopeDigest
		sum.WorkAttestation = att
	}

	return sum, nil
}
