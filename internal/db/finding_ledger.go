package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// InsertFindingLedgerEntry inserts a new finding ledger entry.
func (d *DB) InsertFindingLedgerEntry(entry *types.FindingLedgerEntry) error {
	if entry.ID == "" {
		entry.ID = "fn-" + newID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = now()
	}
	if entry.UpdatedAt == 0 {
		entry.UpdatedAt = entry.CreatedAt
	}
	isBlockingInt := 0
	if entry.IsBlocking {
		isBlockingInt = 1
	}

	_, err := d.sql.Exec(
		`INSERT INTO finding_ledger_entries (
			id, run_id, repo_id, step_name, first_seen_round, first_seen_step_result_id,
			reported_id, fingerprint, severity, action, file, line, description,
			category, check_name, check_id, decision_id, source, user_instructions,
			review_scope, original_finding_json, current_finding_json, status,
			is_blocking, selected_in_round, correcting_commit_sha, fix_session_id, closed_in_round,
			closure_evidence, closure_reason, disposition_provenance,
			last_observed_round, last_observed_file, last_observed_line,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.RunID, entry.RepoID, string(entry.StepName), entry.FirstSeenRound, entry.FirstSeenStepResultID,
		entry.ReportedID, entry.Fingerprint, entry.Severity, entry.Action, entry.File, entry.Line, entry.Description,
		entry.Category, entry.Check, entry.CheckID, entry.DecisionID, entry.Source, entry.UserInstructions,
		entry.ReviewScope, entry.OriginalFindingJSON, entry.CurrentFindingJSON, entry.Status,
		isBlockingInt, entry.SelectedInRound, entry.CorrectingCommitSHA, entry.FixSessionID, entry.ClosedInRound,
		entry.ClosureEvidence, entry.ClosureReason, entry.DispositionProvenance,
		entry.LastObservedRound, entry.LastObservedFile, entry.LastObservedLine,
		entry.CreatedAt, entry.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert finding ledger entry: %w", err)
	}
	return nil
}

// UpdateFindingLedgerEntry updates an existing finding ledger entry.
func (d *DB) UpdateFindingLedgerEntry(entry *types.FindingLedgerEntry) error {
	entry.UpdatedAt = now()
	isBlockingInt := 0
	if entry.IsBlocking {
		isBlockingInt = 1
	}

	res, err := d.sql.Exec(
		`UPDATE finding_ledger_entries SET
			severity = ?, action = ?, file = ?, line = ?, description = ?,
			category = ?, check_name = ?, check_id = ?, decision_id = ?,
			source = ?, user_instructions = ?, review_scope = ?,
			current_finding_json = ?, status = ?, is_blocking = ?,
			selected_in_round = ?, correcting_commit_sha = ?, fix_session_id = ?, closed_in_round = ?,
			closure_evidence = ?, closure_reason = ?, disposition_provenance = ?,
			last_observed_round = ?, last_observed_file = ?, last_observed_line = ?,
			updated_at = ?
		WHERE id = ?`,
		entry.Severity, entry.Action, entry.File, entry.Line, entry.Description,
		entry.Category, entry.Check, entry.CheckID, entry.DecisionID,
		entry.Source, entry.UserInstructions, entry.ReviewScope,
		entry.CurrentFindingJSON, entry.Status, isBlockingInt,
		entry.SelectedInRound, entry.CorrectingCommitSHA, entry.FixSessionID, entry.ClosedInRound,
		entry.ClosureEvidence, entry.ClosureReason, entry.DispositionProvenance,
		entry.LastObservedRound, entry.LastObservedFile, entry.LastObservedLine,
		entry.UpdatedAt, entry.ID,
	)
	if err != nil {
		return fmt.Errorf("update finding ledger entry: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update finding ledger entry rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("finding ledger entry %q not found", entry.ID)
	}
	return nil
}

// RecordFindingLedgerEvent records an observation, transition, or disposition event.
func (d *DB) RecordFindingLedgerEvent(event *types.FindingLedgerEvent) error {
	if event.ID == "" {
		event.ID = "fe-" + newID()
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = now()
	}

	_, err := d.sql.Exec(
		`INSERT INTO finding_ledger_events (
			id, entry_id, run_id, step_name, round, step_result_id,
			event_type, state_before, state_after, commit_sha, session_id,
			evidence, reason, provenance, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.EntryID, event.RunID, string(event.StepName), event.Round, event.StepResultID,
		event.EventType, event.StateBefore, event.StateAfter, event.CommitSHA, event.SessionID,
		event.Evidence, event.Reason, event.Provenance, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("record finding ledger event: %w", err)
	}
	return nil
}

const ledgerEntrySelectCols = `id, run_id, repo_id, step_name, first_seen_round, first_seen_step_result_id,
	reported_id, fingerprint, severity, action, file, line, description,
	category, check_name, check_id, decision_id, source, user_instructions,
	review_scope, original_finding_json, current_finding_json, status,
	is_blocking, COALESCE(selected_in_round, 0), COALESCE(correcting_commit_sha, ''),
	COALESCE(fix_session_id, ''), COALESCE(closed_in_round, 0), COALESCE(closure_evidence, ''),
	COALESCE(closure_reason, ''), COALESCE(disposition_provenance, ''), last_observed_round,
	last_observed_file, last_observed_line, created_at, updated_at`

func scanLedgerEntry(scan func(...interface{}) error) (*types.FindingLedgerEntry, error) {
	e := &types.FindingLedgerEntry{}
	var stepName string
	var isBlockingInt int
	if err := scan(
		&e.ID, &e.RunID, &e.RepoID, &stepName, &e.FirstSeenRound, &e.FirstSeenStepResultID,
		&e.ReportedID, &e.Fingerprint, &e.Severity, &e.Action, &e.File, &e.Line, &e.Description,
		&e.Category, &e.Check, &e.CheckID, &e.DecisionID, &e.Source, &e.UserInstructions,
		&e.ReviewScope, &e.OriginalFindingJSON, &e.CurrentFindingJSON, &e.Status,
		&isBlockingInt, &e.SelectedInRound, &e.CorrectingCommitSHA, &e.FixSessionID,
		&e.ClosedInRound, &e.ClosureEvidence, &e.ClosureReason,
		&e.DispositionProvenance, &e.LastObservedRound, &e.LastObservedFile,
		&e.LastObservedLine, &e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return nil, err
	}
	e.StepName = types.StepName(stepName)
	e.IsBlocking = isBlockingInt != 0
	return e, nil
}

// GetFindingLedgerEntries returns all ledger entries for a run.
func (d *DB) GetFindingLedgerEntries(runID string) ([]*types.FindingLedgerEntry, error) {
	rows, err := d.sql.Query(
		`SELECT `+ledgerEntrySelectCols+` FROM finding_ledger_entries WHERE run_id = ? ORDER BY first_seen_round, id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get finding ledger entries: %w", err)
	}
	defer rows.Close()

	var entries []*types.FindingLedgerEntry
	for rows.Next() {
		entry, err := scanLedgerEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan finding ledger entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// GetFindingLedgerEntriesByStep returns ledger entries for a run and step.
func (d *DB) GetFindingLedgerEntriesByStep(runID string, stepName types.StepName) ([]*types.FindingLedgerEntry, error) {
	rows, err := d.sql.Query(
		`SELECT `+ledgerEntrySelectCols+` FROM finding_ledger_entries WHERE run_id = ? AND step_name = ? ORDER BY first_seen_round, id`,
		runID, string(stepName),
	)
	if err != nil {
		return nil, fmt.Errorf("get finding ledger entries by step: %w", err)
	}
	defer rows.Close()

	var entries []*types.FindingLedgerEntry
	for rows.Next() {
		entry, err := scanLedgerEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan finding ledger entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// GetFindingLedgerEntry returns one ledger entry by ID.
func (d *DB) GetFindingLedgerEntry(id string) (*types.FindingLedgerEntry, error) {
	row := d.sql.QueryRow(
		`SELECT `+ledgerEntrySelectCols+` FROM finding_ledger_entries WHERE id = ?`,
		id,
	)
	entry, err := scanLedgerEntry(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get finding ledger entry: %w", err)
	}
	return entry, nil
}

// GetFindingLedgerEvents returns all events for a ledger entry in chronological order.
func (d *DB) GetFindingLedgerEvents(entryID string) ([]*types.FindingLedgerEvent, error) {
	rows, err := d.sql.Query(
		`SELECT id, entry_id, run_id, step_name, round, COALESCE(step_result_id, ''),
		        event_type, state_before, state_after, COALESCE(commit_sha, ''),
		        COALESCE(session_id, ''), COALESCE(evidence, ''), COALESCE(reason, ''),
		        COALESCE(provenance, ''), created_at
		   FROM finding_ledger_events
		  WHERE entry_id = ?
		  ORDER BY created_at ASC, id ASC`,
		entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("get finding ledger events: %w", err)
	}
	defer rows.Close()

	var events []*types.FindingLedgerEvent
	for rows.Next() {
		ev := &types.FindingLedgerEvent{}
		var stepName string
		if err := rows.Scan(
			&ev.ID, &ev.EntryID, &ev.RunID, &stepName, &ev.Round, &ev.StepResultID,
			&ev.EventType, &ev.StateBefore, &ev.StateAfter, &ev.CommitSHA, &ev.SessionID,
			&ev.Evidence, &ev.Reason, &ev.Provenance, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan finding ledger event: %w", err)
		}
		ev.StepName = types.StepName(stepName)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// GetFindingLedgerEventsByRun returns all events for a run in chronological order.
func (d *DB) GetFindingLedgerEventsByRun(runID string) ([]*types.FindingLedgerEvent, error) {
	rows, err := d.sql.Query(
		`SELECT id, entry_id, run_id, step_name, round, COALESCE(step_result_id, ''),
		        event_type, state_before, state_after, COALESCE(commit_sha, ''),
		        COALESCE(session_id, ''), COALESCE(evidence, ''), COALESCE(reason, ''),
		        COALESCE(provenance, ''), created_at
		   FROM finding_ledger_events
		  WHERE run_id = ?
		  ORDER BY created_at ASC, id ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get finding ledger events by run: %w", err)
	}
	defer rows.Close()

	var events []*types.FindingLedgerEvent
	for rows.Next() {
		ev := &types.FindingLedgerEvent{}
		var stepName string
		if err := rows.Scan(
			&ev.ID, &ev.EntryID, &ev.RunID, &stepName, &ev.Round, &ev.StepResultID,
			&ev.EventType, &ev.StateBefore, &ev.StateAfter, &ev.CommitSHA, &ev.SessionID,
			&ev.Evidence, &ev.Reason, &ev.Provenance, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan finding ledger event: %w", err)
		}
		ev.StepName = types.StepName(stepName)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// GetFindingLedgerSummary compiles a versioned ledger summary for a run.
func (d *DB) GetFindingLedgerSummary(runID string) (*types.FindingLedgerSummary, error) {
	entries, err := d.GetFindingLedgerEntries(runID)
	if err != nil {
		return nil, err
	}
	summary := &types.FindingLedgerSummary{
		ProtocolVersion: types.FindingLedgerProtocolVersion,
		RunID:           runID,
		TotalEntries:    len(entries),
	}
	if len(entries) == 0 {
		return summary, nil
	}

	for _, e := range entries {
		switch e.Status {
		case types.FindingLedgerStatusOpen:
			summary.OpenCount++
		case types.FindingLedgerStatusPendingVerification:
			summary.PendingCount++
		case types.FindingLedgerStatusNeedsReconciliation:
			summary.ReconcileCount++
		default:
			if types.IsClosedLedgerStatus(e.Status) {
				summary.ClosedCount++
			}
		}
		if types.IsUnresolvedLedgerStatus(e.Status) && e.IsBlocking {
			summary.HasBlocking = true
		}
		summary.Entries = append(summary.Entries, types.FindingLedgerEntrySummary{
			ID:                  e.ID,
			StepName:            e.StepName,
			ReportedID:          e.ReportedID,
			Severity:            e.Severity,
			Action:              e.Action,
			File:                e.File,
			Line:                e.Line,
			Description:         e.Description,
			Status:              e.Status,
			IsBlocking:          e.IsBlocking,
			FirstSeenRound:      e.FirstSeenRound,
			LastObservedRound:   e.LastObservedRound,
			CorrectingCommitSHA: e.CorrectingCommitSHA,
			ClosureReason:       e.ClosureReason,
		})
	}
	return summary, nil
}

// migrateLegacyFindingLedger imports pre-ledger historical and active run records
// into finding_ledger_entries and finding_ledger_events idempotently.
func (d *DB) migrateLegacyFindingLedger() error {
	// 1. Identify runs already migrated.
	migratedRuns := make(map[string]bool)
	rows, err := d.sql.Query(`SELECT DISTINCT run_id FROM finding_ledger_entries`)
	if err != nil {
		return fmt.Errorf("check migrated runs: %w", err)
	}
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err == nil {
			migratedRuns[rid] = true
		}
	}
	rows.Close()

	// 2. Query all runs.
	runRows, err := d.sql.Query(`SELECT id, repo_id, status, created_at FROM runs ORDER BY created_at ASC`)
	if err != nil {
		return fmt.Errorf("query runs for migration: %w", err)
	}
	defer runRows.Close()

	type runMeta struct {
		id        string
		repoID    string
		status    string
		createdAt int64
	}
	var runs []runMeta
	for runRows.Next() {
		var rm runMeta
		if err := runRows.Scan(&rm.id, &rm.repoID, &rm.status, &rm.createdAt); err == nil {
			runs = append(runs, rm)
		}
	}
	if err := runRows.Err(); err != nil {
		return err
	}

	for _, r := range runs {
		if migratedRuns[r.id] {
			continue
		}
		if err := d.MigrateLegacyFindingLedgerForRun(r.id); err != nil {
			return fmt.Errorf("migrate run %s: %w", r.id, err)
		}
	}
	return nil
}

// MigrateLegacyFindingLedgerForRun migrates pre-ledger findings for a single run
// if it has not yet been migrated into finding_ledger_entries.
func (d *DB) MigrateLegacyFindingLedgerForRun(runID string) error {
	if runID == "" {
		return nil
	}
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM finding_ledger_entries WHERE run_id = ?`, runID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	run, err := d.GetRun(runID)
	if err != nil || run == nil {
		return err
	}
	return d.migrateOneRun(run.ID, run.RepoID, string(run.Status), run.CreatedAt)
}

func (d *DB) migrateOneRun(runID, repoID, runStatus string, runCreatedAt int64) error {
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		return err
	}

	isActive := runStatus == string(types.RunRunning) || runStatus == string(types.RunPending)

	for _, step := range steps {
		rounds, err := d.GetRoundsByStep(step.ID)
		if err != nil {
			return err
		}
		if len(rounds) == 0 {
			// No rounds; check if step has findings_json directly.
			if step.FindingsJSON != nil && *step.FindingsJSON != "" {
				if findings, parseErr := types.ParseFindingsJSON(*step.FindingsJSON); parseErr == nil {
					for _, item := range findings.Items {
						entryID := "fn-" + newID()
						fingerprint := types.NormalizeFingerprint(item)
						itemJSON, _ := json.Marshal(item)
						status := types.FindingLedgerStatusOpen
						if !isActive {
							status = types.FindingLedgerStatusClosedAccepted
						}
						isBlocking := types.IsBlockingFinding(item)
						e := &types.FindingLedgerEntry{
							ID:                    entryID,
							RunID:                 runID,
							RepoID:                repoID,
							StepName:              step.StepName,
							FirstSeenRound:        1,
							FirstSeenStepResultID: step.ID,
							ReportedID:            item.ID,
							Fingerprint:           fingerprint,
							Severity:              item.Severity,
							Action:                item.ActionOrDefault(),
							File:                  item.File,
							Line:                  item.Line,
							Description:           item.Description,
							Category:              item.Category,
							Check:                 item.Check,
							CheckID:               item.CheckID,
							DecisionID:            item.DecisionID,
							Source:                item.Source,
							UserInstructions:      item.UserInstructions,
							ReviewScope:           item.ReviewScope,
							OriginalFindingJSON:   string(itemJSON),
							CurrentFindingJSON:    string(itemJSON),
							Status:                status,
							IsBlocking:            isBlocking,
							DispositionProvenance: "legacy_migration",
							LastObservedRound:     1,
							LastObservedFile:      item.File,
							LastObservedLine:      item.Line,
							CreatedAt:             runCreatedAt,
							UpdatedAt:             runCreatedAt,
						}
						if err := d.InsertFindingLedgerEntry(e); err != nil {
							return err
						}
						ev := &types.FindingLedgerEvent{
							EntryID:      entryID,
							RunID:        runID,
							StepName:     step.StepName,
							Round:        1,
							StepResultID: step.ID,
							EventType:    types.FindingEventAdmitted,
							StateBefore:  "",
							StateAfter:   status,
							Provenance:   "legacy_migration",
							CreatedAt:    runCreatedAt,
						}
						if err := d.RecordFindingLedgerEvent(ev); err != nil {
							return err
						}
					}
				}
			}
			continue
		}

		// Step has recorded rounds.
		// Track entries created for this step by fingerprint.
		type trackedEntry struct {
			entry             *types.FindingLedgerEntry
			lastSeenRound     int
			seenInLatestRound bool
		}
		byFingerprint := make(map[string]*trackedEntry)

		for _, round := range rounds {
			if round.FindingsJSON == nil || *round.FindingsJSON == "" {
				continue
			}
			findings, parseErr := types.ParseFindingsJSON(*round.FindingsJSON)
			if parseErr != nil {
				continue
			}

			roundCounts := make(map[string]int)
			for _, item := range findings.Items {
				roundCounts[types.NormalizeFingerprint(item)]++
			}

			for _, item := range findings.Items {
				fp := types.NormalizeFingerprint(item)
				existing := byFingerprint[fp]
				// Only match if unambiguous (count == 1).
				if existing != nil && roundCounts[fp] == 1 {
					existing.lastSeenRound = round.Round
					existing.entry.LastObservedRound = round.Round
					existing.entry.LastObservedFile = item.File
					existing.entry.LastObservedLine = item.Line
					itemJSON, _ := json.Marshal(item)
					existing.entry.CurrentFindingJSON = string(itemJSON)
					ev := &types.FindingLedgerEvent{
						EntryID:      existing.entry.ID,
						RunID:        runID,
						StepName:     step.StepName,
						Round:        round.Round,
						StepResultID: step.ID,
						EventType:    types.FindingEventReportedAgain,
						StateBefore:  existing.entry.Status,
						StateAfter:   existing.entry.Status,
						Provenance:   "legacy_migration",
						CreatedAt:    round.CreatedAt,
					}
					_ = d.RecordFindingLedgerEvent(ev)
				} else {
					// New finding or ambiguous match -> assign new distinct ledger ID.
					entryID := "fn-" + newID()
					itemJSON, _ := json.Marshal(item)
					isBlocking := types.IsBlockingFinding(item)
					e := &types.FindingLedgerEntry{
						ID:                    entryID,
						RunID:                 runID,
						RepoID:                repoID,
						StepName:              step.StepName,
						FirstSeenRound:        round.Round,
						FirstSeenStepResultID: step.ID,
						ReportedID:            item.ID,
						Fingerprint:           fp,
						Severity:              item.Severity,
						Action:                item.ActionOrDefault(),
						File:                  item.File,
						Line:                  item.Line,
						Description:           item.Description,
						Category:              item.Category,
						Check:                 item.Check,
						CheckID:               item.CheckID,
						DecisionID:            item.DecisionID,
						Source:                item.Source,
						UserInstructions:      item.UserInstructions,
						ReviewScope:           item.ReviewScope,
						OriginalFindingJSON:   string(itemJSON),
						CurrentFindingJSON:    string(itemJSON),
						Status:                types.FindingLedgerStatusOpen,
						IsBlocking:            isBlocking,
						LastObservedRound:     round.Round,
						LastObservedFile:      item.File,
						LastObservedLine:      item.Line,
						CreatedAt:             round.CreatedAt,
						UpdatedAt:             round.CreatedAt,
					}
					te := &trackedEntry{entry: e, lastSeenRound: round.Round}
					if roundCounts[fp] == 1 {
						byFingerprint[fp] = te
					} else {
						// Ambiguous within the same round: store under a unique key so both are tracked distinctly.
						byFingerprint[fmt.Sprintf("%s|#%s", fp, entryID)] = te
					}
					if err := d.InsertFindingLedgerEntry(e); err != nil {
						return err
					}
					ev := &types.FindingLedgerEvent{
						EntryID:      entryID,
						RunID:        runID,
						StepName:     step.StepName,
						Round:        round.Round,
						StepResultID: step.ID,
						EventType:    types.FindingEventAdmitted,
						StateBefore:  "",
						StateAfter:   types.FindingLedgerStatusOpen,
						Provenance:   "legacy_migration",
						CreatedAt:    round.CreatedAt,
					}
					_ = d.RecordFindingLedgerEvent(ev)
				}
			}
		}

		// Final pass over all tracked entries for this step to determine final status.
		latestRoundNum := 0
		for _, round := range rounds {
			if round.Round > latestRoundNum {
				latestRoundNum = round.Round
			}
		}

		for _, te := range byFingerprint {
			if !isActive {
				// Completed legacy run: preserve historical state without fabricating closure proof.
				if te.lastSeenRound < latestRoundNum {
					// Disappeared before final round in completed run.
					te.entry.Status = types.FindingLedgerStatusClosedAccepted
					te.entry.DispositionProvenance = "legacy_completed_run"
					te.entry.ClosureReason = "historical legacy run resolution"
				} else {
					// Still in final round of completed run (e.g. approved gate with findings).
					te.entry.Status = types.FindingLedgerStatusClosedAccepted
					te.entry.DispositionProvenance = "legacy_completed_run"
					te.entry.ClosureReason = "historical completed run gate acceptance"
				}
				_ = d.UpdateFindingLedgerEntry(te.entry)
			} else {
				// Active pre-ledger run:
				// If a finding was reported in an earlier round but disappeared in a later round
				// without verified closure proof, it MUST BE marked needs_reconciliation!
				if te.lastSeenRound < latestRoundNum {
					te.entry.Status = types.FindingLedgerStatusNeedsReconciliation
					te.entry.ClosureReason = fmt.Sprintf("unverified omission in pre-ledger round (last seen round %d, current round %d); requires reconciliation", te.lastSeenRound, latestRoundNum)
					te.entry.DispositionProvenance = "legacy_active_migration"
					_ = d.UpdateFindingLedgerEntry(te.entry)

					ev := &types.FindingLedgerEvent{
						EntryID:      te.entry.ID,
						RunID:        runID,
						StepName:     step.StepName,
						Round:        latestRoundNum,
						StepResultID: step.ID,
						EventType:    types.FindingEventNeedsReconciliation,
						StateBefore:  types.FindingLedgerStatusOpen,
						StateAfter:   types.FindingLedgerStatusNeedsReconciliation,
						Reason:       te.entry.ClosureReason,
						Provenance:   "legacy_active_migration",
						CreatedAt:    now(),
					}
					_ = d.RecordFindingLedgerEvent(ev)
				}
			}
		}
	}

	return nil
}
