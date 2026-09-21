package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FindingLedger manages the durable per-run finding ledger for a pipeline execution.
type FindingLedger struct {
	db     *db.DB
	runID  string
	repoID string
	mu     sync.Mutex
}

// NewFindingLedger creates a new FindingLedger for the given run.
func NewFindingLedger(database *db.DB, runID, repoID string) *FindingLedger {
	if database != nil && runID != "" {
		_ = database.MigrateLegacyFindingLedgerForRun(runID)
	}
	return &FindingLedger{
		db:     database,
		runID:  runID,
		repoID: repoID,
	}
}

// ProcessRoundFindings updates the durable ledger with the findings produced by one
// execution round, performs verified-fix closure checks, unions unresolved entries,
// and returns the consolidated effective findings JSON.
func (l *FindingLedger) ProcessRoundFindings(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	outcome *StepOutcome,
	fixing bool,
	currentCommitSHA string,
	fixSessionID string,
) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	parsedRoundFindings, _ := types.ParseFindingsJSON(outcome.Findings)
	roundItems := parsedRoundFindings.Items

	// 1. Load existing unresolved entries for this step.
	existingEntries, err := l.db.GetFindingLedgerEntriesByStep(l.runID, stepName)
	if err != nil {
		return outcome.Findings, fmt.Errorf("load existing ledger entries: %w", err)
	}

	var unresolved []*types.FindingLedgerEntry
	for _, e := range existingEntries {
		if types.IsUnresolvedLedgerStatus(e.Status) {
			unresolved = append(unresolved, e)
		}
	}

	// 2. Build coverage and reported findings indexes from this round.
	reviewedPaths := outcome.ReviewedPaths
	reviewablePaths := outcome.ReviewablePaths

	coveredFiles := make(map[string]bool)
	for _, p := range reviewedPaths {
		if norm := normalizeCoveredPath(p); norm != "" {
			coveredFiles[norm] = true
		}
	}
	reviewableFiles := make(map[string]bool)
	for _, p := range reviewablePaths {
		if norm := normalizeCoveredPath(p); norm != "" {
			reviewableFiles[norm] = true
		}
	}
	coverageValid := len(coveredFiles) > 0 && len(reviewableFiles) > 0
	for f := range coveredFiles {
		if !reviewableFiles[f] {
			coverageValid = false
			break
		}
	}

	reportedFiles := make(map[string]bool)
	hasUnanchoredFinding := false
	decisionReviews := make(map[string][]types.DecisionReview)
	for _, dr := range parsedRoundFindings.DecisionReviews {
		decisionReviews[dr.DecisionID] = append(decisionReviews[dr.DecisionID], dr)
	}
	reportedDecisionIDs := make(map[string]bool)

	for _, item := range roundItems {
		if item.DecisionID != "" {
			reportedDecisionIDs[item.DecisionID] = true
		}
		if norm := normalizeCoveredPath(item.File); norm != "" {
			reportedFiles[norm] = true
		} else {
			hasUnanchoredFinding = true
		}
	}

	// 3. Count fingerprints to disambiguate 1-to-1 matches vs ambiguous matches.
	unresolvedCounts := make(map[string]int)
	for _, e := range unresolved {
		unresolvedCounts[e.Fingerprint]++
	}
	roundCounts := make(map[string]int)
	for _, item := range roundItems {
		roundCounts[types.NormalizeFingerprint(item)]++
	}

	pairedRoundIndex := make(map[int]bool)
	pairedLedgerID := make(map[string]bool)

	// Pass 1: exact matches (file, line, description, check, etc.).
	for i, item := range roundItems {
		itemKey := findingKey(item)
		for _, e := range unresolved {
			if pairedLedgerID[e.ID] {
				continue
			}
			var curFinding types.Finding
			_ = json.Unmarshal([]byte(e.CurrentFindingJSON), &curFinding)
			if findingKey(curFinding) == itemKey {
				pairedRoundIndex[i] = true
				pairedLedgerID[e.ID] = true
				l.handleMatchedFinding(e, item, roundNum, stepName, stepResultID)
				break
			}
		}
	}

	// Pass 2: unambiguous fingerprint matches (same file and defect, but line shifted).
	for i, item := range roundItems {
		if pairedRoundIndex[i] {
			continue
		}
		fp := types.NormalizeFingerprint(item)
		if roundCounts[fp] != 1 || unresolvedCounts[fp] != 1 {
			// Ambiguous match across multiple findings; must remain separate.
			continue
		}
		normFile := normalizeCoveredPath(item.File)
		for _, e := range unresolved {
			if pairedLedgerID[e.ID] {
				continue
			}
			if e.Fingerprint == fp && (e.DecisionID != "" || normalizeCoveredPath(e.File) == normFile) {
				pairedRoundIndex[i] = true
				pairedLedgerID[e.ID] = true
				l.handleMatchedFinding(e, item, roundNum, stepName, stepResultID)
				break
			}
		}
	}

	// Pass 3: handle unresolved ledger entries NOT reported in this round.
	for _, e := range unresolved {
		if pairedLedgerID[e.ID] {
			continue
		}

		if e.Status == types.FindingLedgerStatusPendingVerification {
			// Check if verified closure conditions are met.
			canClose := false
			evidence := ""

			// Must have a recorded correcting commit revision.
			// And the session/turn that applied the fix cannot self-certify closure.
			if e.CorrectingCommitSHA != "" && (e.FixSessionID == "" || fixSessionID != e.FixSessionID) {
				if stepName == types.StepReview {
					normFile := normalizeCoveredPath(e.File)
					if e.DecisionID != "" {
						reviews := decisionReviews[e.DecisionID]
						if len(reviews) == 1 && reviews[0].Result == "satisfied" &&
							strings.TrimSpace(reviews[0].Evidence) != "" && !reportedDecisionIDs[e.DecisionID] {
							canClose = true
							evidence = fmt.Sprintf("commit %s, decision %s satisfied: %s", e.CorrectingCommitSHA, e.DecisionID, reviews[0].Evidence)
						}
					} else if coverageValid && !hasUnanchoredFinding && coveredFiles[normFile] && !reportedFiles[normFile] {
						canClose = true
						evidence = fmt.Sprintf("commit %s, covered %s", e.CorrectingCommitSHA, normFile)
					}
				} else if stepName == types.StepTest {
					// Test step: closing verified requires passing scenario / command on tested head.
					if outcome.ExitCode == 0 && (parsedRoundFindings.Verdict == types.TestVerdictGo || parsedRoundFindings.Verdict == "") {
						canClose = true
						evidence = fmt.Sprintf("commit %s, test exit 0 verdict %s", e.CorrectingCommitSHA, parsedRoundFindings.Verdict)
					}
				} else {
					// Lint / other steps with exit 0 and no reported defects.
					if outcome.ExitCode == 0 && len(roundItems) == 0 {
						canClose = true
						evidence = fmt.Sprintf("commit %s, exit 0", e.CorrectingCommitSHA)
					}
				}
			}

			if canClose {
				e.Status = types.FindingLedgerStatusClosedVerified
				e.ClosedInRound = roundNum
				e.ClosureEvidence = evidence
				e.ClosureReason = "verified fixed by independent closure review"
				_ = l.db.UpdateFindingLedgerEntry(e)

				ev := &types.FindingLedgerEvent{
					EntryID:      e.ID,
					RunID:        l.runID,
					StepName:     stepName,
					Round:        roundNum,
					StepResultID: stepResultID,
					EventType:    types.FindingEventClosureReviewed,
					StateBefore:  types.FindingLedgerStatusPendingVerification,
					StateAfter:   types.FindingLedgerStatusClosedVerified,
					CommitSHA:    e.CorrectingCommitSHA,
					Evidence:     evidence,
					Reason:       e.ClosureReason,
				}
				_ = l.db.RecordFindingLedgerEvent(ev)
				continue
			}

			// Not verified closed -> records non-rediscovery and remains pending/open.
			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventNotRediscovered,
				StateBefore:  e.Status,
				StateAfter:   e.Status,
				Reason:       "omitted without verified closure evidence; remains in ledger",
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		} else if stepName == types.StepCI && outcome.ExitCode == 0 && len(roundItems) == 0 {
			stateBefore := e.Status
			e.Status = types.FindingLedgerStatusClosedVerified
			e.ClosedInRound = roundNum
			e.ClosureEvidence = "all CI checks passed"
			e.ClosureReason = "verified fixed by passing CI run"
			_ = l.db.UpdateFindingLedgerEntry(e)

			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventClosureReviewed,
				StateBefore:  stateBefore,
				StateAfter:   types.FindingLedgerStatusClosedVerified,
				Evidence:     e.ClosureEvidence,
				Reason:       e.ClosureReason,
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		} else {
			// e.Status == open or needs_reconciliation
			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventNotRediscovered,
				StateBefore:  e.Status,
				StateAfter:   e.Status,
				Reason:       "not rediscovered in round scan; remains open and blocking",
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		}
	}

	// Pass 4: admit new round items that did not match any unresolved entry.
	for i, item := range roundItems {
		if pairedRoundIndex[i] {
			continue
		}
		itemJSON, _ := json.Marshal(item)
		isBlocking := types.IsBlockingFinding(item)
		newEntry := &types.FindingLedgerEntry{
			ID:                    "fn-" + db.NewID(),
			RunID:                 l.runID,
			RepoID:                l.repoID,
			StepName:              stepName,
			FirstSeenRound:        roundNum,
			FirstSeenStepResultID: stepResultID,
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
			IsBlocking:            isBlocking,
			LastObservedRound:     roundNum,
			LastObservedFile:      item.File,
			LastObservedLine:      item.Line,
		}
		_ = l.db.InsertFindingLedgerEntry(newEntry)

		ev := &types.FindingLedgerEvent{
			EntryID:      newEntry.ID,
			RunID:        l.runID,
			StepName:     stepName,
			Round:        roundNum,
			StepResultID: stepResultID,
			EventType:    types.FindingEventAdmitted,
			StateBefore:  "",
			StateAfter:   types.FindingLedgerStatusOpen,
			CommitSHA:    currentCommitSHA,
		}
		_ = l.db.RecordFindingLedgerEvent(ev)
	}

	// 4. Construct effective findings consolidating all unresolved ledger entries for this step.
	return l.buildEffectiveFindingsJSON(stepName, parsedRoundFindings)
}

func (l *FindingLedger) handleMatchedFinding(
	e *types.FindingLedgerEntry,
	item types.Finding,
	roundNum int,
	stepName types.StepName,
	stepResultID string,
) {
	itemJSON, _ := json.Marshal(item)
	e.LastObservedRound = roundNum
	e.LastObservedFile = item.File
	e.LastObservedLine = item.Line
	e.CurrentFindingJSON = string(itemJSON)
	// Update severity/action/instructions if analyzer or user updated them
	e.Severity = item.Severity
	e.Action = item.ActionOrDefault()
	e.IsBlocking = types.IsBlockingFinding(item)
	if item.UserInstructions != "" {
		e.UserInstructions = item.UserInstructions
	}

	stateBefore := e.Status
	// If it was pending verification and re-reported, the fix failed -> returns to open!
	if e.Status == types.FindingLedgerStatusPendingVerification {
		e.Status = types.FindingLedgerStatusOpen
	}

	_ = l.db.UpdateFindingLedgerEntry(e)

	ev := &types.FindingLedgerEvent{
		EntryID:      e.ID,
		RunID:        l.runID,
		StepName:     stepName,
		Round:        roundNum,
		StepResultID: stepResultID,
		EventType:    types.FindingEventReportedAgain,
		StateBefore:  stateBefore,
		StateAfter:   e.Status,
	}
	_ = l.db.RecordFindingLedgerEvent(ev)
}

// buildEffectiveFindingsJSON compiles all unresolved ledger entries for stepName
// into a types.Findings JSON payload, assigning stable unambiguous IDs.
func (l *FindingLedger) buildEffectiveFindingsJSON(stepName types.StepName, base types.Findings) (string, error) {
	entries, err := l.db.GetFindingLedgerEntriesByStep(l.runID, stepName)
	if err != nil {
		return "", err
	}

	var activeEntries []*types.FindingLedgerEntry
	for _, e := range entries {
		if types.IsUnresolvedLedgerStatus(e.Status) {
			activeEntries = append(activeEntries, e)
		}
	}

	if len(activeEntries) == 0 {
		return "", nil
	}

	summary, err := l.db.GetFindingLedgerSummary(l.runID)
	if err != nil {
		summary = &types.FindingLedgerSummary{
			ProtocolVersion: types.FindingLedgerProtocolVersion,
			RunID:           l.runID,
		}
	}

	// Assign unique, unambiguous display IDs for items while tracking LedgerID.
	usedIDs := make(map[string]bool)
	var items []types.Finding
	for _, e := range activeEntries {
		var f types.Finding
		if err := json.Unmarshal([]byte(e.CurrentFindingJSON), &f); err != nil {
			f = types.Finding{
				Severity:    e.Severity,
				Action:      e.Action,
				File:        e.File,
				Line:        e.Line,
				Description: e.Description,
			}
		}
		f.LedgerID = e.ID

		// Keep original/reported ID if not taken by another active item; otherwise generate unique ID.
		idCandidate := e.ReportedID
		if idCandidate == "" || usedIDs[idCandidate] {
			idCandidate = nextFreeID(string(stepName), usedIDs)
		}
		usedIDs[idCandidate] = true
		f.ID = idCandidate

		items = append(items, f)
	}

	result := types.FindingsMetadata(base)
	result.Items = items
	result.Ledger = summary
	if len(items) == 0 {
		result.Summary = "clean"
	} else if len(items) == 1 {
		result.Summary = "1 open finding"
	} else {
		result.Summary = fmt.Sprintf("%d open findings", len(items))
	}

	return types.MarshalFindingsJSON(result)
}

func nextFreeID(prefix string, used map[string]bool) string {
	for i := 1; ; i++ {
		candidate := prefix + "-" + strconv.Itoa(i)
		if !used[candidate] {
			return candidate
		}
	}
}

// AdmitUserFindings admits user-added findings from a fix action into the ledger
// as pending_verification entries so they carry until verified.
func (l *FindingLedger) AdmitUserFindings(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	added []types.Finding,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, item := range added {
		itemJSON, _ := json.Marshal(item)
		isBlocking := types.IsBlockingFinding(item)
		entryID := "fn-" + db.NewID()
		now := time.Now().Unix()
		reportedID := item.ID
		if reportedID == "" {
			reportedID = "user-" + strconv.Itoa(roundNum)
		}
		newEntry := &types.FindingLedgerEntry{
			ID:                    entryID,
			RunID:                 l.runID,
			RepoID:                l.repoID,
			StepName:              stepName,
			FirstSeenRound:        roundNum,
			FirstSeenStepResultID: stepResultID,
			ReportedID:            reportedID,
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
			Source:                types.FindingSourceUser,
			UserInstructions:      item.UserInstructions,
			ReviewScope:           item.ReviewScope,
			OriginalFindingJSON:   string(itemJSON),
			CurrentFindingJSON:    string(itemJSON),
			Status:                types.FindingLedgerStatusPendingVerification,
			IsBlocking:            isBlocking,
			SelectedInRound:       roundNum,
			LastObservedRound:     roundNum,
			LastObservedFile:      item.File,
			LastObservedLine:      item.Line,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
		if err := l.db.InsertFindingLedgerEntry(newEntry); err != nil {
			return err
		}
		ev := &types.FindingLedgerEvent{
			EntryID:      entryID,
			RunID:        l.runID,
			StepName:     stepName,
			Round:        roundNum,
			StepResultID: stepResultID,
			EventType:    types.FindingEventAdmitted,
			StateBefore:  "",
			StateAfter:   types.FindingLedgerStatusPendingVerification,
			Provenance:   "user_added",
			CreatedAt:    now,
		}
		_ = l.db.RecordFindingLedgerEvent(ev)
	}
	return nil
}

// ProcessSelection marks the selected findings as pending_verification. Unselected
// open findings remain in the open state and stay blocking.
func (l *FindingLedger) ProcessSelection(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	selectedIDs []string,
	source string,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(selectedIDs) == 0 {
		return nil
	}

	entries, err := l.db.GetFindingLedgerEntriesByStep(l.runID, stepName)
	if err != nil {
		return err
	}

	selectedSet := make(map[string]bool, len(selectedIDs))
	for _, id := range selectedIDs {
		if id != "" {
			selectedSet[id] = true
		}
	}

	for _, e := range entries {
		if !types.IsUnresolvedLedgerStatus(e.Status) {
			continue
		}
		// Match by either immutable ledger ID, reported ID, or current finding ID.
		matches := selectedSet[e.ID] || selectedSet[e.ReportedID]
		if !matches {
			var curFinding types.Finding
			if json.Unmarshal([]byte(e.CurrentFindingJSON), &curFinding) == nil {
				matches = selectedSet[curFinding.ID]
			}
		}

		if matches && e.Status == types.FindingLedgerStatusOpen {
			e.Status = types.FindingLedgerStatusPendingVerification
			e.SelectedInRound = roundNum
			if err := l.db.UpdateFindingLedgerEntry(e); err != nil {
				return err
			}

			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventSelectedForCorrection,
				StateBefore:  types.FindingLedgerStatusOpen,
				StateAfter:   types.FindingLedgerStatusPendingVerification,
				Provenance:   source,
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		}
	}
	return nil
}

// RecordCorrectingRevision stamps the correcting commit SHA on all pending_verification entries.
func (l *FindingLedger) RecordCorrectingRevision(
	ctx context.Context,
	stepName types.StepName,
	roundNum int,
	commitSHA string,
	sessionID string,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.db.GetFindingLedgerEntriesByStep(l.runID, stepName)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.Status == types.FindingLedgerStatusPendingVerification {
			e.CorrectingCommitSHA = commitSHA
			e.FixSessionID = sessionID
			_ = l.db.UpdateFindingLedgerEntry(e)

			ev := &types.FindingLedgerEvent{
				EntryID:     e.ID,
				RunID:       l.runID,
				StepName:    stepName,
				Round:       roundNum,
				EventType:   types.FindingEventCorrectionRevisionRecorded,
				StateBefore: e.Status,
				StateAfter:  e.Status,
				CommitSHA:   commitSHA,
				SessionID:   sessionID,
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		}
	}
	return nil
}

// ProcessExplicitDispositionForEntries records an authorized operator or supervisor
// disposition (accepted, not_applicable, or superseded) with provenance and rationale.
func (l *FindingLedger) ProcessExplicitDispositionForEntries(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	entryIDs []string,
	dispositionStatus string,
	reason string,
	provenance string,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.db.GetFindingLedgerEntriesByStep(l.runID, stepName)
	if err != nil {
		return err
	}

	filterMap := make(map[string]bool, len(entryIDs))
	for _, id := range entryIDs {
		filterMap[id] = true
	}

	targetStatus := dispositionStatus
	if targetStatus == "" {
		targetStatus = types.FindingLedgerStatusClosedAccepted
	}
	eventType := types.FindingEventAccepted
	switch targetStatus {
	case types.FindingLedgerStatusClosedNotApplicable:
		eventType = types.FindingEventNotApplicable
	case types.FindingLedgerStatusClosedSuperseded:
		eventType = types.FindingEventSuperseded
	}

	for _, e := range entries {
		if !types.IsUnresolvedLedgerStatus(e.Status) {
			continue
		}
		if len(filterMap) > 0 && !filterMap[e.ID] && !filterMap[e.ReportedID] {
			continue
		}

		stateBefore := e.Status
		e.Status = targetStatus
		e.ClosedInRound = roundNum
		e.ClosureReason = reason
		e.DispositionProvenance = provenance
		_ = l.db.UpdateFindingLedgerEntry(e)

		ev := &types.FindingLedgerEvent{
			EntryID:      e.ID,
			RunID:        l.runID,
			StepName:     stepName,
			Round:        roundNum,
			StepResultID: stepResultID,
			EventType:    eventType,
			StateBefore:  stateBefore,
			StateAfter:   targetStatus,
			Reason:       reason,
			Provenance:   provenance,
		}
		_ = l.db.RecordFindingLedgerEvent(ev)
	}
	return nil
}

// ProcessExplicitDisposition records an authorized operator approval, exception, or skip.
// These are recorded as closed_accepted with provenance and reason, distinct from a verified code fix.
func (l *FindingLedger) ProcessExplicitDisposition(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	action types.ApprovalAction,
	reason string,
	provenance string,
) error {
	targetStatus := types.FindingLedgerStatusClosedAccepted
	return l.ProcessExplicitDispositionForEntries(
		ctx,
		stepName,
		stepResultID,
		roundNum,
		nil,
		targetStatus,
		reason,
		provenance,
	)
}

// AssertAcceptance verifies that the run satisfies the durable finding protocol for
// clean completion. It refuses acceptance if any blocking entry is open, any selected
// correction lacks closure evidence, or any imported history requires reconciliation.
func (l *FindingLedger) AssertAcceptance(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	summary, err := l.db.GetFindingLedgerSummary(l.runID)
	if err != nil {
		return fmt.Errorf("read finding ledger summary: %w", err)
	}

	if summary.ReconcileCount > 0 {
		return fmt.Errorf("refuse terminal acceptance: %d finding(s) require reconciliation from imported history", summary.ReconcileCount)
	}
	if summary.PendingCount > 0 {
		return fmt.Errorf("refuse terminal acceptance: %d finding(s) pending verification without closure proof", summary.PendingCount)
	}
	if summary.OpenCount > 0 && summary.HasBlocking {
		return fmt.Errorf("refuse terminal acceptance: %d blocking finding(s) open in ledger", summary.OpenCount)
	}

	return nil
}
