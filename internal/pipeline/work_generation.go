package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PhaseSnapshot records the filesystem and git state of the worktree before a phase runs.
type PhaseSnapshot struct {
	Phase      types.StepName
	HeadSHA    string
	FileHashes map[string]string // relPath -> type/mode/content identity
}

type poisonEvidence struct {
	phase  types.StepName
	reason string
}

type attestationPublishFunc func(context.Context, *types.WorkAttestation) error

// WriteSetVerdict records the outcome of phase write-set verification.
type WriteSetVerdict struct {
	Allowed           bool
	UnauthorizedPaths []string
	ModifiedPaths     []string
	HeadChanged       bool
	Reason            string
}

// WorkGenerationManager manages the immutable work generations, validation plan,
// phase results, write-set enforcement, transitive invalidations, and final attestation for a run.
type WorkGenerationManager struct {
	db                    *db.DB
	runID                 string
	repoID                string
	workDir               string
	config                *config.Config
	plan                  types.ValidationPlan
	envelope              *types.FinalEnvelope
	attestationPublishers []attestationPublishFunc
	poison                *poisonEvidence
	mu                    sync.Mutex
}

// NewWorkGenerationManager initializes a WorkGenerationManager for a run.
func NewWorkGenerationManager(database *db.DB, runID, repoID, workDir string, cfg *config.Config, configuredSteps ...types.StepName) (*WorkGenerationManager, error) {
	var plan types.ValidationPlan
	if cfg != nil && cfg.ValidationPlan.PlanID != "" {
		plan = cfg.ValidationPlan
	} else {
		plan = config.DefaultConservativeValidationPlan(cfg)
	}

	if plan.ConservativeDefault && configuredSteps != nil {
		stepSet := make(map[types.StepName]bool, len(configuredSteps))
		for _, s := range configuredSteps {
			stepSet[s] = true
		}
		var filtered []types.StepName
		for _, req := range plan.RequiredPhases {
			if stepSet[req] {
				filtered = append(filtered, req)
			}
		}
		plan.RequiredPhases = filtered
		plan.PlanDigest = types.ComputePlanDigest(&plan)
	}

	m := &WorkGenerationManager{
		db:      database,
		runID:   runID,
		repoID:  repoID,
		workDir: workDir,
		config:  cfg,
		plan:    plan,
	}

	if database != nil && runID != "" {
		if err := database.MigrateLegacyWorkGenerationForRun(runID); err != nil {
			return nil, fmt.Errorf("migrate legacy work generation: %w", err)
		}
	}

	return m, nil
}

// Plan returns the trusted validation plan governing this run.
func (m *WorkGenerationManager) Plan() types.ValidationPlan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plan
}

func (m *WorkGenerationManager) RegisterAttestationPublisher(fn func(context.Context, *types.WorkAttestation) error) {
	if m == nil || fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attestationPublishers = append(m.attestationPublishers, fn)
}

// EnsureGeneration ensures that an initial work generation exists for the run,
// creating Generation 0 if none has been persisted yet.
func (m *WorkGenerationManager) EnsureGeneration(ctx context.Context, startingHeadSHA string) (*types.WorkGeneration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.db == nil || m.runID == "" {
		return nil, fmt.Errorf("database and runID required")
	}

	existing, err := m.db.GetCurrentWorkGeneration(m.runID)
	if err != nil {
		return nil, fmt.Errorf("get current work generation: %w", err)
	}
	if existing != nil {
		return existing, nil
	}

	// Compute git tree SHA for starting head
	treeSHA := ""
	if startingHeadSHA != "" && m.workDir != "" {
		out, err := git.Run(ctx, m.workDir, "rev-parse", startingHeadSHA+"^{tree}")
		if err == nil {
			treeSHA = strings.TrimSpace(out)
		}
	}

	// Compute input manifest from disk
	manifest, err := m.computeInputManifestLocked()
	if err != nil {
		return nil, fmt.Errorf("compute input manifest: %w", err)
	}

	depLockDigest := m.computeDependencyLockDigestLocked()
	configDigest := m.computeConfigDigestLocked()
	toolchainDigest := "go-default"

	genDigest := types.ComputeGenerationDigest(
		m.plan.PlanDigest,
		startingHeadSHA,
		treeSHA,
		manifest.Digest,
		m.plan.Commands,
		configDigest,
		toolchainDigest,
		depLockDigest,
		"", // parent digest
		0,  // ordinal 0
		types.GenerationCauseInitial,
	)

	gen0 := &types.WorkGeneration{
		ID:                   "gen-" + m.runID + "-0",
		RunID:                m.runID,
		RepoID:               m.repoID,
		Ordinal:              0,
		Cause:                types.GenerationCauseInitial,
		GenerationDigest:     genDigest,
		PlanID:               m.plan.PlanID,
		PlanDigest:           m.plan.PlanDigest,
		GitHeadSHA:           startingHeadSHA,
		GitTreeSHA:           treeSHA,
		InputManifestDigest:  manifest.Digest,
		InputManifest:        manifest,
		CommandIdentities:    m.plan.Commands,
		ConfigDigest:         configDigest,
		ToolchainDigest:      toolchainDigest,
		DependencyLockDigest: depLockDigest,
		Phase:                types.StepIntent,
		Status:               types.GenerationStatusActive,
		CreatedAt:            time.Now().Unix(),
	}

	if err := m.db.InsertWorkGeneration(gen0); err != nil {
		return nil, fmt.Errorf("persist initial generation 0: %w", err)
	}

	return gen0, nil
}

// CheckPhasePreState captures a snapshot of file content hashes before a phase executes.
func (m *WorkGenerationManager) CheckPhasePreState(ctx context.Context, phase types.StepName) (*PhaseSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.poison != nil {
		return nil, fmt.Errorf("worktree has unresolved unauthorized-write poison from phase %s: %s", m.poison.phase, m.poison.reason)
	}
	if m.db != nil && m.runID != "" {
		phase, reason, err := m.db.GetWorkGenerationPoison(m.runID)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			m.poison = &poisonEvidence{phase: phase, reason: reason}
			return nil, fmt.Errorf("worktree has unresolved unauthorized-write poison from phase %s: %s", phase, reason)
		}
	}

	headSHA := ""
	if m.workDir != "" {
		out, err := git.HeadSHA(ctx, m.workDir)
		if err == nil {
			headSHA = strings.TrimSpace(out)
		}
	}

	hashes, err := m.scanWorkTreeHashesLocked()
	if err != nil {
		return nil, fmt.Errorf("scan worktree pre-state hashes: %w", err)
	}

	return &PhaseSnapshot{
		Phase:      phase,
		HeadSHA:    headSHA,
		FileHashes: hashes,
	}, nil
}

func (m *WorkGenerationManager) RestoreSnapshot(ctx context.Context, pre *PhaseSnapshot) error {
	if m == nil || pre == nil || strings.TrimSpace(m.workDir) == "" {
		return fmt.Errorf("restore phase snapshot: missing worktree or snapshot")
	}
	if strings.TrimSpace(pre.HeadSHA) != "" {
		if _, err := git.Run(ctx, m.workDir, "reset", "--hard", pre.HeadSHA); err != nil {
			return fmt.Errorf("reset worktree to pre-phase head: %w", err)
		}
	} else if _, err := git.Run(ctx, m.workDir, "reset", "--hard"); err != nil {
		return fmt.Errorf("reset worktree: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	restored, err := m.scanWorkTreeHashesLocked()
	if err != nil {
		return fmt.Errorf("scan restored worktree snapshot: %w", err)
	}
	var added []string
	for p := range restored {
		if _, ok := pre.FileHashes[p]; !ok {
			added = append(added, p)
		}
	}
	sort.Slice(added, func(i, j int) bool { return len(added[i]) > len(added[j]) })
	for _, p := range added {
		if err := os.Remove(filepath.Join(m.workDir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove unauthorized write %s: %w", p, err)
		}
	}
	restored, err = m.scanWorkTreeHashesLocked()
	if err != nil {
		return fmt.Errorf("verify restored worktree snapshot: %w", err)
	}
	if !sameSnapshotHashes(pre.FileHashes, restored) {
		reason := "restored worktree does not match pre-phase snapshot"
		m.poison = &poisonEvidence{phase: pre.Phase, reason: reason}
		if m.db != nil && m.runID != "" {
			if err := m.db.SetWorkGenerationPoison(m.runID, pre.Phase, reason); err != nil {
				return err
			}
		}
		return fmt.Errorf("restore phase snapshot: %s", reason)
	}
	return nil
}

// CheckPhasePostState compares pre/post snapshots to enforce phase-scoped write sets.
func (m *WorkGenerationManager) CheckPhasePostState(ctx context.Context, phase types.StepName, fixing bool, pre *PhaseSnapshot) (*WriteSetVerdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	postHashes, err := m.scanWorkTreeHashesLocked()
	if err != nil {
		return nil, fmt.Errorf("scan worktree post-state hashes: %w", err)
	}

	// Find all created, modified, or deleted files
	var modifiedPaths []string
	for p, postHash := range postHashes {
		preHash, exists := pre.FileHashes[p]
		if !exists || preHash != postHash {
			modifiedPaths = append(modifiedPaths, p)
		}
	}
	for p := range pre.FileHashes {
		if _, exists := postHashes[p]; !exists {
			modifiedPaths = append(modifiedPaths, p)
		}
	}

	currentHead := ""
	if m.workDir != "" {
		currentHead, _ = git.HeadSHA(ctx, m.workDir)
	}
	headChanged := currentHead != "" && pre.HeadSHA != "" && currentHead != pre.HeadSHA
	if len(modifiedPaths) == 0 && !headChanged {
		return &WriteSetVerdict{Allowed: true}, nil
	}
	sort.Strings(modifiedPaths)

	// Resolve allowed patterns for phase
	var allowedPatterns []string
	if fixing {
		allowedPatterns = m.plan.PhaseWriteSets[string(phase)+"_fix"]
	} else {
		allowedPatterns = m.plan.PhaseWriteSets[string(phase)]
	}

	var unauthorized []string
	if headChanged && len(allowedPatterns) == 0 {
		unauthorized = append(unauthorized, "git-head")
	}
	for _, p := range modifiedPaths {
		// 1. Engine-protected exclusions: forbidden across ALL phases
		if m.isProtectedExclusion(p) {
			unauthorized = append(unauthorized, p)
			continue
		}

		// 2. Phase-scoped write set check
		if !m.isPathAllowed(p, allowedPatterns) {
			unauthorized = append(unauthorized, p)
		}
	}

	if len(unauthorized) > 0 {
		sort.Strings(unauthorized)
		return &WriteSetVerdict{
			Allowed:           false,
			UnauthorizedPaths: unauthorized,
			ModifiedPaths:     modifiedPaths,
			HeadChanged:       headChanged,
			Reason:            fmt.Sprintf("phase %s made unauthorized writes to %s", phase, strings.Join(unauthorized, ", ")),
		}, nil
	}

	return &WriteSetVerdict{Allowed: true, ModifiedPaths: modifiedPaths, HeadChanged: headChanged}, nil
}

// HandleMutation creates Generation N+1 when selected inputs are mutated, invalidates dependent results,
// and reconciles affected finding closures.
func (m *WorkGenerationManager) HandleMutation(ctx context.Context, cause string, phase types.StepName, newHeadSHA string, mutatedFiles []string) (*types.WorkGeneration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentGen, err := m.db.GetCurrentWorkGeneration(m.runID)
	if err != nil {
		return nil, fmt.Errorf("get current work generation: %w", err)
	}
	if currentGen == nil {
		return nil, fmt.Errorf("cannot mutate without an active generation")
	}

	// Check if any mutated file matches selected inputs
	affectsSelectedInputs := false
	for _, f := range mutatedFiles {
		if m.matchesSelectedInputs(f) {
			affectsSelectedInputs = true
			break
		}
	}

	// If no files specified, inspect working tree directly
	if len(mutatedFiles) == 0 {
		affectsSelectedInputs = true
	}

	if !affectsSelectedInputs {
		// Narrative-only change: preserve generation N, but record new envelope
		treeSHA := ""
		if newHeadSHA != "" && m.workDir != "" {
			out, err := git.Run(ctx, m.workDir, "rev-parse", newHeadSHA+"^{tree}")
			if err == nil {
				treeSHA = strings.TrimSpace(out)
			}
		}
		m.envelope = &types.FinalEnvelope{
			GenerationID:         currentGen.ID,
			GenerationDigest:     currentGen.GenerationDigest,
			FinalHeadSHA:         newHeadSHA,
			FinalTreeSHA:         treeSHA,
			EnvelopeDigest:       types.ComputeEnvelopeDigest(currentGen.GenerationDigest, newHeadSHA, treeSHA),
			NarrativeOnlyChanges: mutatedFiles,
			WriteSetChecksPassed: true,
		}
		return currentGen, nil
	}

	// Seal / supersede current generation N
	if err := m.db.SupersedeWorkGeneration(m.runID, currentGen.ID); err != nil {
		return nil, fmt.Errorf("supersede generation %s: %w", currentGen.ID, err)
	}

	// Invalidate dependent phase results for generation N
	if err := m.db.InvalidateAllActivePhaseResults(m.runID, fmt.Sprintf("invalidated by %s (generation %d -> %d)", cause, currentGen.Ordinal, currentGen.Ordinal+1)); err != nil {
		return nil, fmt.Errorf("invalidate dependent phase results: %w", err)
	}

	// Compute tree SHA for new head
	treeSHA := ""
	if newHeadSHA != "" && m.workDir != "" {
		out, err := git.Run(ctx, m.workDir, "rev-parse", newHeadSHA+"^{tree}")
		if err == nil {
			treeSHA = strings.TrimSpace(out)
		}
	}

	manifest, err := m.computeInputManifestLocked()
	if err != nil {
		return nil, fmt.Errorf("compute input manifest for N+1: %w", err)
	}

	depLockDigest := m.computeDependencyLockDigestLocked()
	configDigest := m.computeConfigDigestLocked()
	toolchainDigest := "go-default"

	newOrdinal := currentGen.Ordinal + 1
	newDigest := types.ComputeGenerationDigest(
		m.plan.PlanDigest,
		newHeadSHA,
		treeSHA,
		manifest.Digest,
		m.plan.Commands,
		configDigest,
		toolchainDigest,
		depLockDigest,
		currentGen.GenerationDigest,
		newOrdinal,
		cause,
	)

	nextGen := &types.WorkGeneration{
		ID:                     fmt.Sprintf("gen-%s-%d", m.runID, newOrdinal),
		RunID:                  m.runID,
		RepoID:                 m.repoID,
		Ordinal:                newOrdinal,
		ParentGenerationID:     currentGen.ID,
		ParentGenerationDigest: currentGen.GenerationDigest,
		Cause:                  cause,
		GenerationDigest:       newDigest,
		PlanID:                 m.plan.PlanID,
		PlanDigest:             m.plan.PlanDigest,
		GitHeadSHA:             newHeadSHA,
		GitTreeSHA:             treeSHA,
		InputManifestDigest:    manifest.Digest,
		InputManifest:          manifest,
		CommandIdentities:      m.plan.Commands,
		ConfigDigest:           configDigest,
		ToolchainDigest:        toolchainDigest,
		DependencyLockDigest:   depLockDigest,
		Phase:                  phase,
		Status:                 types.GenerationStatusActive,
		CreatedAt:              time.Now().Unix(),
	}

	if err := m.db.InsertWorkGeneration(nextGen); err != nil {
		return nil, fmt.Errorf("persist generation %d: %w", newOrdinal, err)
	}

	// Reconcile finding ledger closures: reopen any closed_verified finding whose closure
	// was on an older generation and whose file was mutated (or conservative all if unspecified)
	if err := m.reconcileLedgerClosuresLocked(nextGen.ID, mutatedFiles); err != nil {
		return nil, fmt.Errorf("reconcile finding ledger closures: %w", err)
	}

	return nextGen, nil
}

// RecordPhaseResult records an immutable result for a phase, bound to the current generation.
func (m *WorkGenerationManager) RecordPhaseResult(
	ctx context.Context,
	phase types.StepName,
	status string,
	commandID, evidenceID, outputDigest string,
	dependencies []string,
) (*types.WorkPhaseResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentGen, err := m.db.GetCurrentWorkGeneration(m.runID)
	if err != nil {
		return nil, fmt.Errorf("get current work generation: %w", err)
	}
	if currentGen == nil {
		return nil, fmt.Errorf("cannot record phase result: no active work generation")
	}

	res := &types.WorkPhaseResult{
		ID:                   fmt.Sprintf("res-%s-%s-%d", m.runID, phase, currentGen.Ordinal),
		RunID:                m.runID,
		GenerationID:         currentGen.ID,
		PlanID:               currentGen.PlanID,
		Phase:                phase,
		Status:               status,
		Applicable:           status == types.PhaseResultStatusPassed || status == types.PhaseResultStatusNotApplicable,
		CommandIdentity:      commandID,
		DependencyIdentities: dependencies,
		EvidenceID:           evidenceID,
		OutputDigest:         outputDigest,
		CreatedAt:            time.Now().Unix(),
	}

	if err := m.db.InsertWorkPhaseResult(res); err != nil {
		return nil, fmt.Errorf("persist phase result %s: %w", phase, err)
	}

	return res, nil
}

// AssertAcceptance performs the terminal acceptance check:
// 1. Verifies that all required phases passed and are applicable to the current generation/envelope.
// 2. Confirms no unauthorized writes are outstanding.
// 3. Verifies that CI checked the exact final head.
// 4. Publishes and returns the final WorkAttestation.
func (m *WorkGenerationManager) AssertAcceptance(ctx context.Context, finalHeadSHA string, ciChecksGreen bool) (*types.WorkAttestation, error) {
	ciIdentity := ""
	if ciChecksGreen {
		ciIdentity = "ci_checks_green"
	}
	return m.AssertAcceptanceWithCIEvidence(ctx, finalHeadSHA, ciIdentity)
}

func (m *WorkGenerationManager) AssertAcceptanceWithCIEvidence(ctx context.Context, finalHeadSHA, ciCheckIdentity string) (*types.WorkAttestation, error) {
	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()

	currentGen, err := m.db.GetCurrentWorkGeneration(m.runID)
	if err != nil {
		return nil, fmt.Errorf("load current generation: %w", err)
	}
	if currentGen == nil {
		return nil, fmt.Errorf("terminal acceptance refused: no active work generation exists for run %s", m.runID)
	}

	results, err := m.db.GetWorkPhaseResultsByGeneration(m.runID, currentGen.ID)
	if err != nil {
		return nil, fmt.Errorf("load phase results: %w", err)
	}

	phaseMap := make(map[types.StepName]*types.WorkPhaseResult)
	for _, r := range results {
		phaseMap[r.Phase] = r
	}

	// Verify required phases
	var missing []string
	var stale []string
	for _, req := range m.plan.RequiredPhases {
		res, ok := phaseMap[req]
		if !ok {
			missing = append(missing, string(req))
			continue
		}
		if !res.Applicable {
			stale = append(stale, string(req))
			continue
		}
		if res.Status != types.PhaseResultStatusPassed && res.Status != types.PhaseResultStatusNotApplicable {
			return nil, fmt.Errorf("terminal acceptance refused: phase %s status is %q (requires passed)", req, res.Status)
		}
		if res.Status == types.PhaseResultStatusNotApplicable && m.plan.Reason == "" {
			return nil, fmt.Errorf("terminal acceptance refused: phase %s marked not_applicable without explicit trusted-plan reason", req)
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("terminal acceptance refused: missing required phase results: %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		return nil, fmt.Errorf("terminal acceptance refused: stale required phase results: %s", strings.Join(stale, ", "))
	}

	// Resolve final tree SHA
	finalTreeSHA := ""
	if finalHeadSHA != "" && m.workDir != "" {
		out, err := git.Run(ctx, m.workDir, "rev-parse", finalHeadSHA+"^{tree}")
		if err == nil {
			finalTreeSHA = strings.TrimSpace(out)
		}
	}
	if finalTreeSHA == "" {
		finalTreeSHA = currentGen.GitTreeSHA
	}

	ciCheckIdentity = strings.TrimSpace(ciCheckIdentity)
	if ciCheckIdentity == "" {
		return nil, fmt.Errorf("terminal acceptance refused: missing CI evidence identity")
	}
	switch ciCheckIdentity {
	case "ci_checks_green", "ci_approval_override", "declared_no_ci", "ci_not_required":
	default:
		return nil, fmt.Errorf("terminal acceptance refused: unsupported CI evidence identity %q", ciCheckIdentity)
	}

	envelopeDigest := types.ComputeEnvelopeDigest(currentGen.GenerationDigest, finalHeadSHA, finalTreeSHA)

	ledgerSummary, err := m.db.GetFindingLedgerSummary(m.runID)
	if err != nil {
		return nil, fmt.Errorf("load finding ledger summary: %w", err)
	}
	allResults, err := m.db.GetWorkPhaseResultsByRun(m.runID)
	if err != nil {
		return nil, fmt.Errorf("load invalidation history: %w", err)
	}
	var invalidationHistory []types.WorkInvalidationRecord
	for _, r := range allResults {
		if r.InvalidatedAt == nil && strings.TrimSpace(r.InvalidationReason) == "" {
			continue
		}
		at := int64(0)
		if r.InvalidatedAt != nil {
			at = *r.InvalidatedAt
		}
		invalidationHistory = append(invalidationHistory, types.WorkInvalidationRecord{
			Phase:      r.Phase,
			ResultID:   r.ID,
			FromStatus: types.PhaseResultStatusPassed,
			ToStatus:   r.Status,
			Reason:     r.InvalidationReason,
			MutatedBy:  currentGen.Cause,
			At:         at,
		})
	}

	var summaries []types.WorkPhaseResultSummary
	evidenceMap := make(map[string]string)
	for _, r := range results {
		summaries = append(summaries, types.WorkPhaseResultSummary{
			ID:                 r.ID,
			Phase:              r.Phase,
			Status:             r.Status,
			Applicable:         r.Applicable,
			EvidenceID:         r.EvidenceID,
			OutputDigest:       r.OutputDigest,
			InvalidationReason: r.InvalidationReason,
		})
		if r.EvidenceID != "" {
			evidenceMap[string(r.Phase)] = r.EvidenceID
		}
	}

	att := &types.WorkAttestation{
		ID:                  "att-" + m.runID,
		ProtocolVersion:     types.WorkGenerationProtocolVersion,
		RunID:               m.runID,
		GenerationID:        currentGen.ID,
		GenerationDigest:    currentGen.GenerationDigest,
		GenerationOrdinal:   currentGen.Ordinal,
		PlanID:              m.plan.PlanID,
		PlanDigest:          m.plan.PlanDigest,
		FinalHeadSHA:        finalHeadSHA,
		FinalTreeSHA:        finalTreeSHA,
		FinalEnvelopeDigest: envelopeDigest,
		PhaseResults:        summaries,
		EvidenceIdentities:  evidenceMap,
		LedgerSummary:       ledgerSummary,
		InvalidationHistory: invalidationHistory,
		CIHeadSHA:           finalHeadSHA,
		CICheckIdentity:     ciCheckIdentity,
		CreatedAt:           time.Now().Unix(),
	}
	att.AttestationDigest = types.ComputeAttestationDigest(att)

	existingAtt, err := m.db.GetWorkAttestation(m.runID)
	if err != nil {
		return nil, fmt.Errorf("load existing final attestation: %w", err)
	}
	if existingAtt != nil {
		if existingAtt.AttestationDigest != att.AttestationDigest || existingAtt.GenerationDigest != att.GenerationDigest || existingAtt.FinalHeadSHA != att.FinalHeadSHA {
			return nil, fmt.Errorf("persist final attestation: existing attestation %s does not match current acceptance", existingAtt.ID)
		}
		att = existingAtt
	} else if err := m.db.InsertWorkAttestation(att); err != nil {
		return nil, fmt.Errorf("persist final attestation: %w", err)
	}

	// Seal generation
	_ = m.db.SealWorkGeneration(m.runID, currentGen.ID, types.StepCI)

	publishers := append([]attestationPublishFunc(nil), m.attestationPublishers...)
	locked = false
	m.mu.Unlock()
	for _, publish := range publishers {
		if err := publish(ctx, att); err != nil {
			return nil, fmt.Errorf("publish final work attestation: %w", err)
		}
	}

	return att, nil
}

// Summary returns a publishable summary of the current work generation state.
func (m *WorkGenerationManager) Summary(ctx context.Context) (*types.WorkGenerationSummary, error) {
	if m.db == nil || m.runID == "" {
		return nil, nil
	}
	return m.db.GetWorkGenerationSummary(m.runID)
}

// Attestation returns the work attestation for the run, if any.
func (m *WorkGenerationManager) Attestation(ctx context.Context) (*types.WorkAttestation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db == nil || m.runID == "" {
		return nil, nil
	}
	return m.db.GetWorkAttestation(m.runID)
}

// computeInputManifestLocked scans workDir and constructs the canonical InputManifest.
func (m *WorkGenerationManager) computeInputManifestLocked() (*types.InputManifest, error) {
	if m.workDir == "" {
		return &types.InputManifest{Digest: "empty"}, nil
	}

	var entries []types.InputManifestEntry

	err := filepath.WalkDir(m.workDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(m.workDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		// Always skip .git and internal cache dirs
		if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Check against selected input patterns
		if !m.matchesSelectedInputs(relSlash) {
			return nil
		}

		// Check file type and symlink safety
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", relSlash, err)
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", relSlash, err)
			}
			// Reject symlink substituting executable input or pointing outside
			if !filepath.IsLocal(target) || strings.HasPrefix(target, "..") {
				return fmt.Errorf("unauthorized symlink substitution for executable input %q pointing outside worktree to %q", relSlash, target)
			}
			targetHash := sha256.Sum256([]byte(target))
			entries = append(entries, types.InputManifestEntry{
				Path:   relSlash,
				Mode:   uint32(fi.Mode().Perm()),
				Type:   "symlink",
				Digest: hex.EncodeToString(targetHash[:]),
				Size:   fi.Size(),
			})
			return nil
		}

		if !fi.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %s (mode %s)", relSlash, fi.Mode())
		}

		// Compute file content hash
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("open %s: %w", relSlash, err)
		}
		defer f.Close()

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return fmt.Errorf("hash %s: %w", relSlash, err)
		}
		digest := hex.EncodeToString(h.Sum(nil))

		entries = append(entries, types.InputManifestEntry{
			Path:   relSlash,
			Mode:   uint32(fi.Mode().Perm()),
			Type:   "regular",
			Digest: digest,
			Size:   fi.Size(),
		})

		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	digest := types.ComputeInputManifestDigest(entries)
	return &types.InputManifest{
		Entries: entries,
		Digest:  digest,
	}, nil
}

// scanWorkTreeHashesLocked collects path->identity mappings for every non-directory entry in workDir.
func (m *WorkGenerationManager) scanWorkTreeHashesLocked() (map[string]string, error) {
	hashes := make(map[string]string)
	if m.workDir == "" {
		return hashes, nil
	}

	err := filepath.WalkDir(m.workDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(m.workDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		if relSlash == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
		} else if strings.HasPrefix(relSlash, ".git/") {
			return nil
		}

		if d.IsDir() {
			return nil
		}

		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", relSlash, err)
		}

		mode := fi.Mode()
		switch {
		case mode.IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("open %s: %w", relSlash, err)
			}
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				_ = f.Close()
				return fmt.Errorf("hash %s: %w", relSlash, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", relSlash, err)
			}
			hashes[relSlash] = fmt.Sprintf("regular:%o:%d:%s", uint32(mode.Perm()), fi.Size(), hex.EncodeToString(h.Sum(nil)))
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", relSlash, err)
			}
			h := sha256.Sum256([]byte(target))
			hashes[relSlash] = fmt.Sprintf("symlink:%o:%d:%s", uint32(mode.Perm()), fi.Size(), hex.EncodeToString(h[:]))
		default:
			hashes[relSlash] = fmt.Sprintf("other:%s:%d:%d", mode.String(), fi.Size(), fi.ModTime().UnixNano())
		}

		return nil
	})

	return hashes, err
}

func (m *WorkGenerationManager) computeDependencyLockDigestLocked() string {
	if m.workDir == "" {
		return ""
	}
	for _, lockFile := range []string{"go.sum", "package-lock.json", "yarn.lock", "Cargo.lock"} {
		p := filepath.Join(m.workDir, lockFile)
		if data, err := os.ReadFile(p); err == nil {
			h := sha256.Sum256(data)
			return hex.EncodeToString(h[:])
		}
	}
	return "no-lock"
}

func (m *WorkGenerationManager) computeConfigDigestLocked() string {
	if m.workDir == "" {
		return ""
	}
	cfgFile := filepath.Join(m.workDir, ".no-mistakes.yaml")
	if data, err := os.ReadFile(cfgFile); err == nil {
		h := sha256.Sum256(data)
		return hex.EncodeToString(h[:])
	}
	return "default-config"
}

func isNarrativePath(file string) bool {
	clean := filepath.ToSlash(file)
	base := path.Base(clean)
	if strings.HasPrefix(clean, "docs/") ||
		strings.HasSuffix(clean, ".md") ||
		strings.HasSuffix(clean, ".txt") ||
		strings.HasPrefix(base, "README") ||
		strings.HasPrefix(base, "LICENSE") ||
		strings.HasPrefix(base, "CONTRIBUTING") ||
		strings.HasPrefix(base, "CHANGELOG") {
		return true
	}
	return false
}

// MatchesSelectedInputs reports whether a relative path matches the validation plan's selected inputs.
func (m *WorkGenerationManager) MatchesSelectedInputs(file string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.matchesSelectedInputs(file)
}

// SealGeneration seals the current active generation at the given phase.
func (m *WorkGenerationManager) SealGeneration(ctx context.Context, phase types.StepName) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentGen, err := m.db.GetCurrentWorkGeneration(m.runID)
	if err != nil {
		return err
	}
	if currentGen == nil {
		return nil
	}
	return m.db.SealWorkGeneration(m.runID, currentGen.ID, phase)
}

// CurrentGeneration returns the current active work generation for the run.
func (m *WorkGenerationManager) CurrentGeneration(ctx context.Context) (*types.WorkGeneration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db.GetCurrentWorkGeneration(m.runID)
}

func (m *WorkGenerationManager) matchesSelectedInputs(file string) bool {
	if isNarrativePath(file) {
		for _, pat := range m.plan.SelectedInputs {
			if pat != "**" && pat != "**/*" && pat != "*" && matchPattern(file, pat) {
				return true
			}
		}
		return false
	}
	for _, pat := range m.plan.SelectedInputs {
		if matchPattern(file, pat) {
			return true
		}
	}
	return false
}

func (m *WorkGenerationManager) isProtectedExclusion(file string) bool {
	file = strings.ToLower(file)
	for _, pat := range m.plan.ProtectedExclusions {
		if matchPattern(file, strings.ToLower(pat)) {
			return true
		}
	}
	return false
}

func (m *WorkGenerationManager) isPathAllowed(file string, allowedPatterns []string) bool {
	if len(allowedPatterns) == 0 {
		return false
	}
	for _, pat := range allowedPatterns {
		if matchPattern(file, pat) {
			return true
		}
	}
	return false
}

func sameSnapshotHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func matchPattern(file, pattern string) bool {
	file = filepath.ToSlash(file)
	pattern = filepath.ToSlash(pattern)

	if pattern == "**" || pattern == "**/*" || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if !strings.Contains(suffix, "/") {
			matched, _ := path.Match(suffix, path.Base(file))
			return matched
		}
		return strings.HasSuffix(file, suffix) || strings.Contains(file, "/"+suffix)
	}
	if !strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, path.Base(file))
		return matched
	}
	matched, _ := path.Match(pattern, file)
	return matched
}

func (m *WorkGenerationManager) reconcileLedgerClosuresLocked(newGenID string, mutatedFiles []string) error {
	if m.db == nil || m.runID == "" {
		return nil
	}

	entries, err := m.db.GetFindingLedgerEntries(m.runID)
	if err != nil {
		return err
	}

	mutatedMap := make(map[string]bool)
	for _, f := range mutatedFiles {
		mutatedMap[f] = true
	}

	for _, e := range entries {
		if e.Status == types.FindingLedgerStatusClosedVerified {
			// If file was mutated (or all if empty list)
			if len(mutatedFiles) == 0 || mutatedMap[e.File] || mutatedMap[e.LastObservedFile] {
				event := &types.FindingLedgerEvent{
					ID:           "fe-" + e.ID + "-" + newGenID,
					EntryID:      e.ID,
					RunID:        m.runID,
					StepName:     e.StepName,
					Round:        e.LastObservedRound,
					EventType:    types.FindingEventGenerationInvalidated,
					StateBefore:  types.FindingLedgerStatusClosedVerified,
					StateAfter:   types.FindingLedgerStatusOpen,
					Reason:       fmt.Sprintf("closure invalidated by mutation in generation %s", newGenID),
					Provenance:   "generation_mutation",
					GenerationID: newGenID,
					CreatedAt:    time.Now().Unix(),
				}
				e.Status = types.FindingLedgerStatusOpen
				e.ClosureEvidence = ""
				e.ClosureReason = ""
				e.ClosureGenerationID = ""
				e.UpdatedAt = time.Now().Unix()

				if err := m.db.UpdateFindingLedgerEntryWithEvent(e, event); err != nil {
					return fmt.Errorf("reopen finding %s on generation invalidation: %w", e.ID, err)
				}
			}
		}
	}

	return nil
}
