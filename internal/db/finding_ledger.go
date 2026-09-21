package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

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
	return updateLedgerEntryExec(d.sql, entry)
}

// InsertFindingLedgerEntryWithEvent atomically inserts an entry and its provenance event.
func (d *DB) InsertFindingLedgerEntryWithEvent(entry *types.FindingLedgerEntry, event *types.FindingLedgerEvent) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin finding ledger entry/event insert: %w", err)
	}
	defer tx.Rollback()
	if err := insertLedgerEntryTx(tx, entry); err != nil {
		return err
	}
	if err := insertLedgerEventTx(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finding ledger entry/event insert: %w", err)
	}
	return nil
}

// UpdateFindingLedgerEntryWithEvent atomically updates an entry and records its provenance event.
func (d *DB) UpdateFindingLedgerEntryWithEvent(entry *types.FindingLedgerEntry, event *types.FindingLedgerEvent) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin finding ledger entry/event update: %w", err)
	}
	defer tx.Rollback()
	if err := updateLedgerEntryExec(tx, entry); err != nil {
		return err
	}
	if err := insertLedgerEventTx(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finding ledger entry/event update: %w", err)
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
//
// The counts cover every entry the run has ever recorded, but Entries carries
// only the UNRESOLVED ones (open, pending verification, reconciliation
// required). That is the protocol's publishable surface - "what still blocks
// this run" - and keeping it to unresolved entries bounds every consumer that
// embeds a summary: the per-step findings JSON, the run/step IPC payloads, and
// the axi gate and status renders. Closed entries stay fully readable in the
// database (GetFindingLedgerEntries) and in the step's own round history; they
// are counted here rather than re-published, so a long review loop cannot grow
// a status frame without bound.
func (d *DB) GetFindingLedgerSummary(runID string) (*types.FindingLedgerSummary, error) {
	return d.findingLedgerSummary(runID, "")
}

// GetFindingLedgerSummaryForStep is GetFindingLedgerSummary restricted to one
// step's entries. A step's own findings payload reports what that step still
// has to dispose of, without claiming entries owned by another step.
func (d *DB) GetFindingLedgerSummaryForStep(runID string, stepName types.StepName) (*types.FindingLedgerSummary, error) {
	return d.findingLedgerSummary(runID, stepName)
}

func (d *DB) findingLedgerSummary(runID string, stepName types.StepName) (*types.FindingLedgerSummary, error) {
	var (
		entries []*types.FindingLedgerEntry
		err     error
	)
	if stepName == "" {
		entries, err = d.GetFindingLedgerEntries(runID)
	} else {
		entries, err = d.GetFindingLedgerEntriesByStep(runID, stepName)
	}
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
		if !types.IsUnresolvedLedgerStatus(e.Status) {
			continue
		}
		if e.IsBlocking {
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

// MigrateLegacyFindingLedgerForRun imports a pre-ledger active run's persisted
// round and finding records into the durable ledger exactly once.
//
// Idempotency is owned by the finding_ledger_migrations marker, not by the
// presence of imported entries: a crash midway through an import leaves partial
// entries behind, and re-running must complete that run's history rather than
// skip it. The whole import (entries, events, and the marker) is one
// transaction, so an interrupted import leaves nothing behind at all and a
// retry starts clean. A terminal run is refused outright: completed history is
// historical and is never relabelled here.
func (d *DB) MigrateLegacyFindingLedgerForRun(runID string) error {
	if runID == "" {
		return nil
	}
	var migrated int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM finding_ledger_migrations WHERE run_id = ?`, runID).Scan(&migrated); err != nil {
		return fmt.Errorf("check finding ledger migration marker: %w", err)
	}
	if migrated > 0 {
		return nil
	}
	run, err := d.GetRun(runID)
	if err != nil || run == nil {
		return err
	}
	if types.RunStatus(run.Status).Terminal() {
		return nil
	}

	// Plan first, write second. The plan's reads must finish before the write
	// transaction opens: SQLite serializes a writer against readers on other
	// pooled connections, so a read through d.sql inside the transaction would
	// deadlock against the transaction itself.
	entries, events, err := d.planLegacyMigration(run.ID, run.RepoID, run.CreatedAt)
	if err != nil {
		return err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin finding ledger migration: %w", err)
	}
	defer tx.Rollback()

	if err := writeLegacyMigrationTx(tx, entries, events); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO finding_ledger_migrations (run_id, protocol_version, imported_at) VALUES (?, ?, ?)`,
		runID, types.FindingLedgerProtocolVersion, now(),
	); err != nil {
		return fmt.Errorf("record finding ledger migration marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finding ledger migration: %w", err)
	}
	return nil
}

// planLegacyMigration reads one active run's persisted step/round findings and
// produces the ledger entries and events an import would write.
//
// It is deliberately read-only and completes before any transaction opens. The
// import's reads (steps, rounds) and its writes (entries, events, marker) must
// not overlap: SQLite serializes a writer against readers on other pooled
// connections, so reading through d.sql while a write transaction is open
// deadlocks against the import's own transaction.
//
// Two shapes are produced, and neither invents a closure:
//
//   - A finding reported in an earlier round but absent from the run's latest
//     round becomes needs_reconciliation. The pre-ledger engine could not say
//     why it stopped being reported, so the imported entry says exactly that
//     instead of guessing "fixed".
//   - Everything else stays open and blocking. The live ledger then disposes of
//     it through the ordinary round/selection/closure path.
//
// A completed run never reaches this function (see MigrateLegacyFindingLedgerForRun).
func (d *DB) planLegacyMigration(runID, repoID string, runCreatedAt int64) ([]*types.FindingLedgerEntry, []*types.FindingLedgerEvent, error) {
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		return nil, nil, err
	}

	var entries []*types.FindingLedgerEntry
	var events []*types.FindingLedgerEvent
	for _, step := range steps {
		rounds, err := d.GetRoundsByStep(step.ID)
		if err != nil {
			return nil, nil, err
		}
		stepEntries, stepEvents, err := planStepMigration(runID, repoID, step, rounds, runCreatedAt)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, stepEntries...)
		events = append(events, stepEvents...)
	}
	return entries, events, nil
}

// planStepMigration plans one step's import from either its findings_json (no
// rounds recorded) or its round history.
func planStepMigration(runID, repoID string, step *StepResult, rounds []*StepRound, runCreatedAt int64) ([]*types.FindingLedgerEntry, []*types.FindingLedgerEvent, error) {
	if len(rounds) == 0 {
		return planStepFindingsMigration(runID, repoID, step, runCreatedAt)
	}
	return planStepRoundsMigration(runID, repoID, step, rounds, runCreatedAt)
}

// planStepFindingsMigration imports a step that holds findings without round rows.
func planStepFindingsMigration(runID, repoID string, step *StepResult, runCreatedAt int64) ([]*types.FindingLedgerEntry, []*types.FindingLedgerEvent, error) {
	if step.FindingsJSON == nil || *step.FindingsJSON == "" {
		return nil, nil, nil
	}
	findings, parseErr := types.ParseFindingsJSON(*step.FindingsJSON)
	if parseErr != nil {
		return nil, nil, nil
	}
	var entries []*types.FindingLedgerEntry
	var events []*types.FindingLedgerEvent
	for _, item := range findings.Items {
		if types.IsStepOwnedFinding(item) {
			// Step-owned operator parks keep their existing owner and are never
			// imported; the step re-derives them from live state. See
			// types.IsStepOwnedFinding.
			continue
		}
		entry := legacyEntry(runID, repoID, step, step.StepName, 1, item, runCreatedAt)
		entries = append(entries, entry)
		events = append(events, legacyAdmittedEvent(entry, step.ID, 1, runCreatedAt))
	}
	return entries, events, nil
}

// planStepRoundsMigration walks a step's rounds oldest-first, preserving the
// distinction between "reported again" and "a new, distinct finding".
func planStepRoundsMigration(runID, repoID string, step *StepResult, rounds []*StepRound, runCreatedAt int64) ([]*types.FindingLedgerEntry, []*types.FindingLedgerEvent, error) {
	type trackedEntry struct {
		entry         *types.FindingLedgerEntry
		lastSeenRound int
	}
	byFingerprint := make(map[string]*trackedEntry)

	latestRoundNum := 0
	for _, round := range rounds {
		if round.Round > latestRoundNum {
			latestRoundNum = round.Round
		}
	}

	var entries []*types.FindingLedgerEntry
	var events []*types.FindingLedgerEvent
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
			if types.IsStepOwnedFinding(item) {
				// Step-owned operator parks are re-derived from live state by
				// their step and are never imported. See types.IsStepOwnedFinding.
				continue
			}
			fp := types.NormalizeFingerprint(item)
			existing := byFingerprint[fp]
			// Only an unambiguous 1-to-1 fingerprint match continues an entry.
			// Anything else is a distinct finding, so two rounds that both called
			// something F1 are never merged by position or by a shared label.
			if existing != nil && roundCounts[fp] == 1 {
				existing.lastSeenRound = round.Round
				existing.entry.LastObservedRound = round.Round
				existing.entry.LastObservedFile = item.File
				existing.entry.LastObservedLine = item.Line
				itemJSON, _ := json.Marshal(item)
				existing.entry.CurrentFindingJSON = string(itemJSON)
				existing.entry.UpdatedAt = round.CreatedAt
				events = append(events, &types.FindingLedgerEvent{
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
				})
				continue
			}

			entry := legacyEntry(runID, repoID, step, step.StepName, round.Round, item, round.CreatedAt)
			entries = append(entries, entry)
			events = append(events, legacyAdmittedEvent(entry, step.ID, round.Round, round.CreatedAt))
			te := &trackedEntry{entry: entry, lastSeenRound: round.Round}
			if roundCounts[fp] == 1 {
				byFingerprint[fp] = te
			} else {
				// Ambiguous within the same round: track each distinctly so neither
				// absorbs the other's later observation.
				byFingerprint[fmt.Sprintf("%s|#%s", fp, entry.ID)] = te
			}
		}
	}

	// An active run's pre-ledger omission is unexplained, so it is imported as
	// reconciliation-required rather than inferred closed. Sorted by entry ID so
	// the plan (and therefore the retry) is deterministic.
	var omitted []*trackedEntry
	for _, te := range byFingerprint {
		if te.lastSeenRound < latestRoundNum {
			omitted = append(omitted, te)
		}
	}
	sort.Slice(omitted, func(i, j int) bool { return omitted[i].entry.ID < omitted[j].entry.ID })
	for _, te := range omitted {
		reason := fmt.Sprintf("unverified omission in pre-ledger round (last seen round %d, current round %d); requires reconciliation", te.lastSeenRound, latestRoundNum)
		te.entry.Status = types.FindingLedgerStatusNeedsReconciliation
		te.entry.ClosureReason = reason
		te.entry.DispositionProvenance = "legacy_active_migration"
		// The entry pointer is already in entries: it was appended when the
		// finding was first admitted. Mutating it updates the plan in place.
		events = append(events, &types.FindingLedgerEvent{
			EntryID:     te.entry.ID,
			RunID:       runID,
			StepName:    step.StepName,
			Round:       latestRoundNum,
			EventType:   types.FindingEventNeedsReconciliation,
			StateBefore: types.FindingLedgerStatusOpen,
			StateAfter:  types.FindingLedgerStatusNeedsReconciliation,
			Reason:      reason,
			Provenance:  "legacy_active_migration",
			CreatedAt:   now(),
		})
	}
	return entries, events, nil
}

func legacyAdmittedEvent(entry *types.FindingLedgerEntry, stepResultID string, round int, createdAt int64) *types.FindingLedgerEvent {
	return &types.FindingLedgerEvent{
		EntryID:      entry.ID,
		RunID:        entry.RunID,
		StepName:     entry.StepName,
		Round:        round,
		StepResultID: stepResultID,
		EventType:    types.FindingEventAdmitted,
		StateBefore:  "",
		StateAfter:   types.FindingLedgerStatusOpen,
		Provenance:   "legacy_migration",
		CreatedAt:    createdAt,
	}
}

// legacyEntry builds the ledger entry an imported pre-ledger finding becomes.
// It is always open: nothing in the pre-ledger record is acceptance evidence.
func legacyEntry(runID, repoID string, step *StepResult, stepName types.StepName, round int, item types.Finding, createdAt int64) *types.FindingLedgerEntry {
	itemJSON, _ := json.Marshal(item)
	return &types.FindingLedgerEntry{
		ID:                    "fn-" + newID(),
		RunID:                 runID,
		RepoID:                repoID,
		StepName:              stepName,
		FirstSeenRound:        round,
		FirstSeenStepResultID: step.ID,
		ReportedID:            item.ID,
		Fingerprint:           types.NormalizeFingerprint(item),
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
		IsBlocking:            types.IsBlockingFinding(item),
		LastObservedRound:     round,
		LastObservedFile:      item.File,
		LastObservedLine:      item.Line,
		CreatedAt:             createdAt,
		UpdatedAt:             createdAt,
	}
}

// writeLegacyMigrationTx writes a planned import as one transaction. Callers
// must have finished every read before arriving here.
func writeLegacyMigrationTx(tx *sql.Tx, entries []*types.FindingLedgerEntry, events []*types.FindingLedgerEvent) error {
	for _, entry := range entries {
		if err := insertLedgerEntryTx(tx, entry); err != nil {
			return err
		}
	}
	for _, event := range events {
		if err := insertLedgerEventTx(tx, event); err != nil {
			return err
		}
	}
	return nil
}

type ledgerExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func updateLedgerEntryExec(exec ledgerExec, entry *types.FindingLedgerEntry) error {
	entry.UpdatedAt = now()
	isBlockingInt := 0
	if entry.IsBlocking {
		isBlockingInt = 1
	}
	res, err := exec.Exec(
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

// insertLedgerEntryTx and insertLedgerEventTx are the transactional half of the
// ledger writers, used so an import is all-or-nothing. The non-transactional
// methods above are the live round paths.
func insertLedgerEntryTx(tx *sql.Tx, entry *types.FindingLedgerEntry) error {
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
	_, err := tx.Exec(
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

func insertLedgerEventTx(tx *sql.Tx, event *types.FindingLedgerEvent) error {
	if event.ID == "" {
		event.ID = "fe-" + newID()
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = now()
	}
	_, err := tx.Exec(
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
