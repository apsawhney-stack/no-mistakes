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
//
// The guard inputs come from durable evidence rather than from the caller: the
// fixing session was recorded with the correcting revision, and this round's
// certifier identity and resumption flag ride the step outcome. That is what
// makes "the session that applied the correction cannot certify it" a check
// instead of an assumption.
func (l *FindingLedger) ProcessRoundFindings(
	ctx context.Context,
	stepName types.StepName,
	stepResultID string,
	roundNum int,
	outcome *StepOutcome,
	currentCommitSHA string,
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

	// The turn that produced THIS round's outcome, used as the closure
	// certifier's identity.
	certifierSessionID := ""
	certifierResumed := false
	if outcome != nil {
		certifierSessionID = strings.TrimSpace(outcome.AgentSessionID)
		certifierResumed = outcome.AgentSessionResumed
	}

	// A CI check observation is a live measurement, not a claim about code, and
	// the CI step owns its finding set: a fresh SETTLED observation replaces the
	// previous one (AGENTS.md, CI Step Findings Model). So a settled clean CI
	// observation supersedes the check-anchored entries it no longer reports. It
	// is recorded as an operator-visible supersession naming the observed head,
	// never as a verified fix, so a red that simply stopped reproducing stays
	// distinguishable from a proven repair in statistics and output. A
	// non-clean or unresolved CI round supersedes nothing, and a review-bot
	// comment is not a check observation at all, so it keeps its own lifecycle
	// (it parks as an ask-user finding until a human decides).
	ciSettledClean := stepName == types.StepCI && outcome != nil &&
		!outcome.NeedsApproval && outcome.ExitCode == 0 && len(roundItems) == 0

	// Pass 3: handle unresolved ledger entries NOT reported in this round.
	for _, e := range unresolved {
		if pairedLedgerID[e.ID] {
			continue
		}

		// A selected, repaired CI check keeps the verified-closure path: its
		// published repair plus the check's own re-run at the corrected revision
		// is a real proof and belongs in the verified count. Supersession covers
		// only entries no correction was ever dispatched for.
		if ciSettledClean && e.Status != types.FindingLedgerStatusPendingVerification && supersededByCICleanObservation(e) {
			stateBefore := e.Status
			e.Status = types.FindingLedgerStatusClosedSuperseded
			e.ClosedInRound = roundNum
			e.ClosureReason = "superseded by a fresh settled CI observation"
			e.ClosureEvidence = fmt.Sprintf("settled CI observation at head %s reported no failing check", shortCommit(currentCommitSHA))
			e.DispositionProvenance = "ci_settled_observation"
			_ = l.db.UpdateFindingLedgerEntry(e)

			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventSuperseded,
				StateBefore:  stateBefore,
				StateAfter:   types.FindingLedgerStatusClosedSuperseded,
				CommitSHA:    currentCommitSHA,
				Evidence:     e.ClosureEvidence,
				Reason:       e.ClosureReason,
				Provenance:   e.DispositionProvenance,
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
			continue
		}

		if e.Status == types.FindingLedgerStatusPendingVerification {
			// Closure is a proof, not a conclusion. It requires all of: a
			// recorded correcting revision, a closure review of THAT revision,
			// and a certifier that is not the session which applied the fix.
			canClose := false
			evidence := ""
			refusal := ""

			switch {
			case e.CorrectingCommitSHA == "":
				// The fix round never recorded the revision it produced, so there
				// is nothing a closure review could be a review OF.
				refusal = "no recorded correcting revision"
			case !l.certifierIsIndependent(e, certifierSessionID, certifierResumed):
				refusal = fmt.Sprintf("certified by the correcting session %s", e.FixSessionID)
			case currentCommitSHA != "" && currentCommitSHA != e.CorrectingCommitSHA:
				// The reviewed head is not the corrected revision: the review that
				// would close this entry did not look at the fix.
				refusal = fmt.Sprintf("closure review is of %s, not the correcting revision %s", shortCommit(currentCommitSHA), shortCommit(e.CorrectingCommitSHA))
			default:
				canClose, evidence = l.closureEvidenceFor(e, stepName, outcome, closureContext{
					roundItems:          roundItems,
					parsedRoundFindings: parsedRoundFindings,
					coverageValid:       coverageValid,
					coveredFiles:        coveredFiles,
					reportedFiles:       reportedFiles,
					hasUnanchored:       hasUnanchoredFinding,
					decisionReviews:     decisionReviews,
					reportedDecisionIDs: reportedDecisionIDs,
				})
				if !canClose && refusal == "" {
					refusal = "closure review did not positively cover the corrected revision"
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
					SessionID:    certifierSessionID,
					Evidence:     evidence,
					Reason:       e.ClosureReason,
				}
				_ = l.db.RecordFindingLedgerEvent(ev)
				continue
			}

			// Not verified closed -> records non-rediscovery and remains pending.
			ev := &types.FindingLedgerEvent{
				EntryID:      e.ID,
				RunID:        l.runID,
				StepName:     stepName,
				Round:        roundNum,
				StepResultID: stepResultID,
				EventType:    types.FindingEventNotRediscovered,
				StateBefore:  e.Status,
				StateAfter:   e.Status,
				Reason:       fmt.Sprintf("omitted without verified closure (%s); remains pending verification", refusal),
			}
			_ = l.db.RecordFindingLedgerEvent(ev)
		} else {
			// An open or reconciliation-required entry stays exactly where it is.
			// An empty or partial round scan is silence, and silence never closes
			// a blocking finding: only selection plus verified closure, or an
			// explicit disposition, may.
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
	// Step-owned findings (a Test budget cut, a failing configured test command)
	// are passed through unresolved instead of admitted: they are re-derived from
	// live state each round, so their absence from a later round is a
	// re-measurement, not an analyzer omission. See types.IsStepOwnedFinding.
	var unledgered []types.Finding
	for i, item := range roundItems {
		if pairedRoundIndex[i] {
			continue
		}
		if types.IsStepOwnedFinding(item) {
			unledgered = append(unledgered, item)
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
	return l.buildEffectiveFindingsJSON(stepName, outcome.Findings, parsedRoundFindings, unledgered, outcome.ReviewedPaths)
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
	// A reopened entry keeps its content identity but must lose correction
	// evidence: the revision it named was reviewed and the defect survived, so
	// that evidence can never be reused to close the finding later.
	if e.Status == types.FindingLedgerStatusPendingVerification {
		e.Status = types.FindingLedgerStatusOpen
		e.CorrectingCommitSHA = ""
		e.FixSessionID = ""
		e.ClosureEvidence = ""
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

// certifierIsIndependent reports whether the turn that produced this round's
// outcome may certify closure of e. The session that applied a correction is
// never allowed to certify it; neither is a turn that cannot be told apart from
// it. The answer is deliberately asymmetric:
//
//   - A run/adapter that never minted a durable fix session (e.FixSessionID is
//     empty) has no self-certification risk to guard: every one of its turns is
//     a cold invocation. Non-review fixes run session-free by construction
//     (SessionRole is unset outside the review fixer).
//   - A certifier that did not CONTINUE a session ran cold. A cold turn is a
//     fresh invocation and therefore independent of every earlier one, whatever
//     identity its adapter reports - reporting a session identity is not the
//     same as resuming one, and some adapters (and fixtures) reuse a value.
//   - Only a certifier that both resumed a session and reports the fixing
//     session's identity is refused.
//
// The executor supplies the fixing session from the durable review-fixer role
// and the certifier from the outcome of the turn that produced the closure
// review, so the guard is real evidence rather than a field nobody fills. The
// invocations that certify closure - the review rereview and the Test evidence
// turn - run session-free by construction (see RunSessions), which is exactly
// why this can be checked rather than assumed: routing one of them through the
// fixing role session is what would make this refuse.
func (l *FindingLedger) certifierIsIndependent(e *types.FindingLedgerEntry, certifier string, certifierResumed bool) bool {
	fixer := strings.TrimSpace(e.FixSessionID)
	certifier = strings.TrimSpace(certifier)
	if fixer == "" || !certifierResumed {
		return true
	}
	return certifier != fixer
}

// closureContext carries the current round's positive-verification evidence.
type closureContext struct {
	roundItems          []types.Finding
	parsedRoundFindings types.Findings
	coverageValid       bool
	coveredFiles        map[string]bool
	reportedFiles       map[string]bool
	hasUnanchored       bool
	decisionReviews     map[string][]types.DecisionReview
	reportedDecisionIDs map[string]bool
}

// closureEvidenceFor decides whether this round positively certifies that the
// corrected revision no longer carries e, and describes the evidence if so.
//
// One shape per step, because "what would prove this is gone" differs by step:
// a review proves it through coverage of the corrected file with no defect
// re-reported, a test through a passing live-validation verdict recorded on the
// correcting revision, and every deterministic step through re-running clean on
// it. All of them share the same refusals applied by the caller: no correcting
// revision, a non-independent certifier, or a review of some other revision.
func (l *FindingLedger) closureEvidenceFor(e *types.FindingLedgerEntry, stepName types.StepName, outcome *StepOutcome, cc closureContext) (bool, string) {
	if outcome == nil {
		return false, ""
	}
	switch stepName {
	case types.StepReview:
		if e.DecisionID != "" {
			reviews := cc.decisionReviews[e.DecisionID]
			if len(reviews) == 1 && reviews[0].Result == "satisfied" &&
				strings.TrimSpace(reviews[0].Evidence) != "" && !cc.reportedDecisionIDs[e.DecisionID] {
				return true, fmt.Sprintf("commit %s, fresh review satisfied decision %s: %s",
					shortCommit(e.CorrectingCommitSHA), e.DecisionID, reviews[0].Evidence)
			}
			return false, ""
		}
		normFile := normalizeCoveredPath(e.File)
		if normFile == "" {
			return false, ""
		}
		if cc.coverageValid && !cc.hasUnanchored && cc.coveredFiles[normFile] && !cc.reportedFiles[normFile] {
			return true, fmt.Sprintf("commit %s, fresh review covered %s and re-reported no defect in it",
				shortCommit(e.CorrectingCommitSHA), normFile)
		}
		return false, ""
	case types.StepTest:
		// The reproducer must have been driven on the corrected revision: a
		// pass on any other head says nothing about this fix.
		if outcome.ExitCode != 0 || cc.parsedRoundFindings.Verdict != types.TestVerdictGo {
			return false, ""
		}
		testedHead := strings.TrimSpace(cc.parsedRoundFindings.TestedHeadSHA)
		if testedHead != "" && testedHead != e.CorrectingCommitSHA {
			return false, ""
		}
		return true, fmt.Sprintf("commit %s, live-validation verdict %q", shortCommit(e.CorrectingCommitSHA), types.TestVerdictGo)
	default:
		// Deterministic steps (lint, document, push, custom gates) prove closure
		// by re-running clean on the corrected revision: exit 0 with nothing
		// re-reported. The caller has already required the pending state, the
		// recorded correcting revision, and that this round is that revision.
		if outcome.ExitCode != 0 || len(cc.roundItems) != 0 {
			return false, ""
		}
		return true, fmt.Sprintf("commit %s, step re-ran clean (exit 0, no findings)", shortCommit(e.CorrectingCommitSHA))
	}
}

// supersededByCICleanObservation reports whether a clean, settled CI
// observation could have re-reported this entry, and therefore replaces it.
// Only checks the CI step actually observes qualify: a failing job, a transient
// that resolved, or a merge conflict that no longer reproduces. A review-bot
// comment rides the same step but is a human decision, not a check result.
func supersededByCICleanObservation(e *types.FindingLedgerEntry) bool {
	switch e.Category {
	case types.FindingCategoryCICheck, types.FindingCategoryCITransient, types.FindingCategoryCIMergeConflict:
		return true
	default:
		return false
	}
}

// shortCommit renders a commit for human-readable evidence, keeping the first
// twelve characters so a closure record names the revision without pasting a
// full SHA into every event.
func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// buildEffectiveFindingsJSON compiles this step's unresolved ledger entries into
// a types.Findings payload, assigning stable display IDs and carrying each
// entry's immutable ledger ID.
//
// The payload always preserves the round's own evidence metadata - reviewed
// paths, decision reviews, live-validation verdict, scenarios, artifacts, risk -
// because that metadata is the step's product even when it has no items. Only
// the items are replaced by the unresolved ledger set. Two properties depend on
// this: a Test step that found nothing must still publish its verdict (the PR
// attestation's live_validation lives there), and a clean review round must
// still publish its coverage record (a missing one parks the gate). Dropping
// either by returning an empty payload would erase real evidence.
//
// An empty string is returned only when the round produced no findings payload
// at all AND nothing is unresolved, which is the one case with nothing to say.
func (l *FindingLedger) buildEffectiveFindingsJSON(stepName types.StepName, roundRaw string, base types.Findings, unledgered []types.Finding, reviewedPaths []string) (string, error) {
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

	if len(activeEntries) == 0 && len(unledgered) == 0 && strings.TrimSpace(roundRaw) == "" {
		return "", nil
	}

	summary, err := l.db.GetFindingLedgerSummaryForStep(l.runID, stepName)
	if err != nil {
		summary = &types.FindingLedgerSummary{
			ProtocolVersion: types.FindingLedgerProtocolVersion,
			RunID:           l.runID,
		}
	}

	// Assign unique, unambiguous display IDs for items while tracking LedgerID.
	displayIDs := assignDisplayIDs(string(stepName), activeEntries)
	usedIDs := make(map[string]bool, len(activeEntries))
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
		f.ID = displayIDs[e.ID]
		usedIDs[f.ID] = true

		items = append(items, f)
	}

	// Step-owned findings this round reported are carried verbatim: the ledger
	// does not own them, but the gate and the refusal checks still must see
	// them, so dropping them here would silently un-refuse an approval the
	// pipeline deliberately refuses.
	for _, f := range unledgered {
		if f.ID == "" || usedIDs[f.ID] {
			f.ID = nextFreeID(string(stepName), usedIDs)
		}
		usedIDs[f.ID] = true
		items = append(items, f)
	}

	result := types.FindingsMetadata(base)
	// The step's own coverage record is authoritative for this round and wins
	// over whatever the round's payload happened to carry - the same precedence
	// the pre-ledger carry merge applied - so a step that reports coverage on
	// its outcome is never silently uncovered.
	if len(reviewedPaths) > 0 {
		result.ReviewedPaths = append([]string(nil), reviewedPaths...)
	}
	result.Items = items
	if summary.TotalEntries > 0 {
		result.Ledger = summary
	}
	switch {
	case len(items) == 1:
		result.Summary = "1 open finding"
	case len(items) > 1:
		result.Summary = fmt.Sprintf("%d open findings", len(items))
	}

	return types.MarshalFindingsJSON(result)
}

// ledgerRequiresDisposition reports whether a step's effective findings payload
// still carries an unresolved ledger entry. The executor parks on this in
// addition to the finding-severity checks, so an entry the protocol has not
// disposed of - including a non-blocking one that could otherwise complete its
// step silently and then refuse terminal acceptance - always reaches a gate
// instead of dead-ending the run.
func ledgerRequiresDisposition(raw string) bool {
	if raw == "" {
		return false
	}
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	return parsed.Ledger.Unresolved() > 0
}

func nextFreeID(prefix string, used map[string]bool) string {
	for i := 1; ; i++ {
		candidate := prefix + "-" + strconv.Itoa(i)
		if !used[candidate] {
			return candidate
		}
	}
}

// assignDisplayIDs maps each entry to the display ID the operator sees for it
// this round: the analyzer's own label when it is still free, otherwise a
// step-scoped minted one. Display IDs are presentation only - the durable
// identity is the ledger ID - but a selection response names a display ID, so
// the assignment must be reproducible server-side rather than stored.
//
// One owner matters: two entries that share an analyzer label (two rounds that
// both said "review-1") mean the second one's display ID is derived, and
// resolving a selection against the raw reported label instead would select the
// first entry by its label while the operator meant the second by its display
// ID. Both the payload builder and the selection resolver go through here.
func assignDisplayIDs(stepName string, entries []*types.FindingLedgerEntry) map[string]string {
	used := make(map[string]bool, len(entries))
	assigned := make(map[string]string, len(entries))
	for _, e := range entries {
		candidate := e.ReportedID
		if candidate == "" || used[candidate] {
			candidate = nextFreeID(stepName, used)
		}
		used[candidate] = true
		assigned[e.ID] = candidate
	}
	return assigned
}

// resolveLedgerSelection resolves the IDs an operator selected to ledger entry
// IDs. It accepts the immutable ledger ID and the display ID this round shows,
// so every accepted selector names the same entry in the fixer payload and the
// ledger state transition.
func (l *FindingLedger) resolveLedgerSelection(stepName types.StepName, entries []*types.FindingLedgerEntry, selectedIDs []string) map[string]bool {
	selected := make(map[string]bool, len(selectedIDs))
	for _, id := range selectedIDs {
		if id != "" {
			selected[id] = true
		}
	}
	matched := make(map[string]bool)
	if len(selected) == 0 {
		return matched
	}
	unresolved := make([]*types.FindingLedgerEntry, 0, len(entries))
	for _, e := range entries {
		if types.IsUnresolvedLedgerStatus(e.Status) {
			unresolved = append(unresolved, e)
		}
	}
	displayIDs := assignDisplayIDs(string(stepName), unresolved)
	for _, e := range unresolved {
		if selected[e.ID] || selected[displayIDs[e.ID]] {
			matched[e.ID] = true
		}
	}
	return matched
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

	matched := l.resolveLedgerSelection(stepName, entries, selectedIDs)

	for _, e := range entries {
		if !types.IsUnresolvedLedgerStatus(e.Status) {
			continue
		}
		if matched[e.ID] && e.Status == types.FindingLedgerStatusOpen {
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

	matched := map[string]bool(nil)
	if len(entryIDs) > 0 {
		matched = l.resolveLedgerSelection(stepName, entries, entryIDs)
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
		if matched != nil && !matched[e.ID] {
			continue
		}

		stateBefore := e.Status
		e.Status = targetStatus
		e.ClosedInRound = roundNum
		e.ClosureReason = reason
		e.DispositionProvenance = provenance
		// A reconciliation-required entry is closed by an operator's decision,
		// not by proof: that decision IS the reconciliation. Record it so a
		// reader can tell an accepted ambiguity from a verified fix, and never
		// as closed_verified.
		if stateBefore == types.FindingLedgerStatusNeedsReconciliation {
			if strings.TrimSpace(e.ClosureReason) == "" {
				e.ClosureReason = "reconciliation-required entry disposed by operator decision"
			}
			e.DispositionProvenance = provenance + ":reconciled"
		}
		if err := l.db.UpdateFindingLedgerEntry(e); err != nil {
			return err
		}

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
		if err := l.db.RecordFindingLedgerEvent(ev); err != nil {
			return err
		}
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
