package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Work generation protocol version.
const WorkGenerationProtocolVersion = "v1"

// Work generation status constants.
const (
	GenerationStatusActive     = "active"
	GenerationStatusSealed     = "sealed"
	GenerationStatusSuperseded = "superseded"
)

// Work generation cause constants.
const (
	GenerationCauseInitial                   = "initial"
	GenerationCauseSourceMutation            = "source_mutation"
	GenerationCauseTestMutation              = "test_mutation"
	GenerationCauseReviewFix                 = "review_fix"
	GenerationCauseTestFix                   = "test_fix"
	GenerationCauseLintFix                   = "lint_fix"
	GenerationCausePlanMutation              = "plan_mutation"
	GenerationCauseConfigMutation            = "config_mutation"
	GenerationCauseDependencyMutation        = "dependency_mutation"
	GenerationCauseToolchainMutation         = "toolchain_mutation"
	GenerationCauseUnauthorizedWriteRecovery = "unauthorized_write_recovery"
	GenerationCauseLegacyAdoption            = "legacy_adoption"
)

// Work phase result status constants.
const (
	PhaseResultStatusPassed        = "passed"
	PhaseResultStatusFailed        = "failed"
	PhaseResultStatusStale         = "stale"
	PhaseResultStatusInvalidated   = "invalidated"
	PhaseResultStatusNotApplicable = "not_applicable"
	PhaseResultStatusPending       = "pending"
)

// Write set verdict constants.
const (
	WriteSetVerdictClean        = "clean"
	WriteSetVerdictUnauthorized = "unauthorized"
)

// FindingIDUnauthorizedWriteRefusal is the step-owned finding when an agent
// or mutating phase attempted unauthorized writes outside its declared write set.
const FindingIDUnauthorizedWriteRefusal = "unauthorized-write-refusal"

// ValidationPlan defines the trusted rules governing candidate execution:
// selected executable inputs, required phases, deterministic commands,
// dependency edges, and phase-scoped write sets.
type ValidationPlan struct {
	Version             string              `json:"version" yaml:"version"`
	PlanID              string              `json:"plan_id" yaml:"plan_id"`
	PlanDigest          string              `json:"plan_digest" yaml:"plan_digest"`
	SelectedInputs      []string            `json:"selected_inputs" yaml:"selected_inputs"`
	RequiredPhases      []StepName          `json:"required_phases" yaml:"required_phases"`
	Commands            map[string]string   `json:"commands,omitempty" yaml:"commands,omitempty"`
	DependencyEdges     map[string][]string `json:"dependency_edges,omitempty" yaml:"dependency_edges,omitempty"`
	PhaseWriteSets      map[string][]string `json:"phase_write_sets,omitempty" yaml:"phase_write_sets,omitempty"`
	ProtectedExclusions []string            `json:"protected_exclusions,omitempty" yaml:"protected_exclusions,omitempty"`
	ConservativeDefault bool                `json:"conservative_default" yaml:"conservative_default"`
	Reason              string              `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// InputManifestEntry represents a single verified input file in the working tree.
type InputManifestEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Type   string `json:"type"` // "regular" or "symlink"
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// InputManifest is the sorted collection of verified input entries.
type InputManifest struct {
	Entries []InputManifestEntry `json:"entries"`
	Digest  string               `json:"digest"`
}

// WorkGeneration represents an immutable per-run generation of validated work.
type WorkGeneration struct {
	ID                     string            `json:"id"`
	RunID                  string            `json:"run_id"`
	RepoID                 string            `json:"repo_id"`
	Ordinal                int               `json:"ordinal"`
	ParentGenerationID     string            `json:"parent_generation_id,omitempty"`
	ParentGenerationDigest string            `json:"parent_generation_digest,omitempty"`
	Cause                  string            `json:"cause"`
	GenerationDigest       string            `json:"generation_digest"`
	PlanID                 string            `json:"plan_id"`
	PlanDigest             string            `json:"plan_digest"`
	PlanYAML               string            `json:"plan_yaml,omitempty"`
	GitHeadSHA             string            `json:"git_head_sha"`
	GitTreeSHA             string            `json:"git_tree_sha"`
	InputManifestDigest    string            `json:"input_manifest_digest"`
	InputManifest          *InputManifest    `json:"input_manifest,omitempty"`
	CommandIdentities      map[string]string `json:"command_identities,omitempty"`
	ConfigDigest           string            `json:"config_digest,omitempty"`
	ToolchainDigest        string            `json:"toolchain_digest,omitempty"`
	DependencyLockDigest   string            `json:"dependency_lock_digest,omitempty"`
	Phase                  StepName          `json:"phase"`
	Status                 string            `json:"status"`
	CreatedAt              int64             `json:"created_at"`
}

// WorkPhaseResult represents an immutable validation result bound to one generation and plan.
type WorkPhaseResult struct {
	ID                   string   `json:"id"`
	RunID                string   `json:"run_id"`
	GenerationID         string   `json:"generation_id"`
	PlanID               string   `json:"plan_id"`
	Phase                StepName `json:"phase"`
	Status               string   `json:"status"`
	Applicable           bool     `json:"applicable"`
	CommandIdentity      string   `json:"command_identity,omitempty"`
	DependencyIdentities []string `json:"dependency_identities,omitempty"`
	EvidenceID           string   `json:"evidence_id,omitempty"`
	OutputDigest         string   `json:"output_digest,omitempty"`
	InvalidationReason   string   `json:"invalidation_reason,omitempty"`
	CreatedAt            int64    `json:"created_at"`
	InvalidatedAt        *int64   `json:"invalidated_at,omitempty"`
}

// WorkPhaseResultSummary is a compact representation of a phase result.
type WorkPhaseResultSummary struct {
	ID                 string   `json:"id"`
	Phase              StepName `json:"phase"`
	Status             string   `json:"status"`
	Applicable         bool     `json:"applicable"`
	EvidenceID         string   `json:"evidence_id,omitempty"`
	OutputDigest       string   `json:"output_digest,omitempty"`
	InvalidationReason string   `json:"invalidation_reason,omitempty"`
}

// WorkInvalidationRecord logs one invalidation transition.
type WorkInvalidationRecord struct {
	Phase      StepName `json:"phase"`
	ResultID   string   `json:"result_id"`
	FromStatus string   `json:"from_status"`
	ToStatus   string   `json:"to_status"`
	Reason     string   `json:"reason"`
	MutatedBy  string   `json:"mutated_by"`
	At         int64    `json:"at"`
}

// FinalEnvelope records the final repository envelope when narrative-only
// changes occur after the content generation was sealed.
type FinalEnvelope struct {
	GenerationID         string   `json:"generation_id"`
	GenerationDigest     string   `json:"generation_digest"`
	FinalHeadSHA         string   `json:"final_head_sha"`
	FinalTreeSHA         string   `json:"final_tree_sha"`
	EnvelopeDigest       string   `json:"envelope_digest"`
	NarrativeOnlyChanges []string `json:"narrative_only_changes,omitempty"`
	WriteSetChecksPassed bool     `json:"write_set_checks_passed"`
}

// WorkAttestation binds the terminal validation proof to the exact content,
// trusted plan, results, finding ledger, and CI head.
type WorkAttestation struct {
	ID                  string                   `json:"id"`
	ProtocolVersion     string                   `json:"protocol_version"`
	RunID               string                   `json:"run_id"`
	GenerationID        string                   `json:"generation_id"`
	GenerationDigest    string                   `json:"generation_digest"`
	GenerationOrdinal   int                      `json:"generation_ordinal"`
	PlanID              string                   `json:"plan_id"`
	PlanDigest          string                   `json:"plan_digest"`
	FinalHeadSHA        string                   `json:"final_head_sha"`
	FinalTreeSHA        string                   `json:"final_tree_sha"`
	FinalEnvelopeDigest string                   `json:"final_envelope_digest"`
	PhaseResults        []WorkPhaseResultSummary `json:"phase_results"`
	EvidenceIdentities  map[string]string        `json:"evidence_identities,omitempty"`
	LedgerSummary       *FindingLedgerSummary    `json:"ledger_summary,omitempty"`
	InvalidationHistory []WorkInvalidationRecord `json:"invalidation_history,omitempty"`
	CIHeadSHA           string                   `json:"ci_head_sha"`
	CICheckIdentity     string                   `json:"ci_check_identity,omitempty"`
	AttestationDigest   string                   `json:"attestation_digest"`
	CreatedAt           int64                    `json:"created_at"`
}

// WorkGenerationSummary is the publishable IPC/status representation of the
// current work-generation state for a run.
type WorkGenerationSummary struct {
	ProtocolVersion          string                   `json:"protocol_version"`
	RunID                    string                   `json:"run_id"`
	CurrentGenerationOrdinal int                      `json:"current_generation_ordinal"`
	CurrentGenerationID      string                   `json:"current_generation_id"`
	CurrentGenerationDigest  string                   `json:"current_generation_digest"`
	ParentGenerationID       string                   `json:"parent_generation_id,omitempty"`
	MutationCause            string                   `json:"mutation_cause,omitempty"`
	PlanID                   string                   `json:"plan_id"`
	PlanVersion              string                   `json:"plan_version"`
	PlanDigest               string                   `json:"plan_digest"`
	WriteSetVerdict          string                   `json:"write_set_verdict"`
	UnauthorizedWrites       []string                 `json:"unauthorized_writes,omitempty"`
	PhaseResults             []WorkPhaseResultSummary `json:"phase_results,omitempty"`
	MissingRequirements      []string                 `json:"missing_requirements,omitempty"`
	StaleRequirements        []string                 `json:"stale_requirements,omitempty"`
	AttestationID            string                   `json:"attestation_id,omitempty"`
	AttestationDigest        string                   `json:"attestation_digest,omitempty"`
	FinalEnvelopeDigest      string                   `json:"final_envelope_digest,omitempty"`
	WorkAttestation          *WorkAttestation         `json:"work_attestation,omitempty"`
	LedgerRelationship       string                   `json:"ledger_relationship,omitempty"`
}

// ComputePlanDigest calculates a deterministic canonical SHA-256 digest of a validation plan.
func ComputePlanDigest(plan *ValidationPlan) string {
	if plan == nil {
		return ""
	}
	// Copy plan without PlanDigest to avoid circular dependency
	clone := *plan
	clone.PlanDigest = ""

	// Ensure sorted slices for determinism
	sort.Strings(clone.SelectedInputs)
	sort.Slice(clone.RequiredPhases, func(i, j int) bool {
		return clone.RequiredPhases[i] < clone.RequiredPhases[j]
	})
	sort.Strings(clone.ProtectedExclusions)

	data, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// ComputeInputManifestDigest calculates a deterministic canonical SHA-256 digest
// of input manifest entries. Entries are sorted by relative path.
func ComputeInputManifestDigest(entries []InputManifestEntry) string {
	sorted := make([]InputManifestEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})
	data, err := json.Marshal(sorted)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// ComputeGenerationDigest calculates a deterministic canonical SHA-256 digest
// over the immutable generation content. Wall time, volatile paths, and host metadata
// are strictly excluded.
func ComputeGenerationDigest(
	planDigest, gitHeadSHA, gitTreeSHA, inputManifestDigest string,
	commandIdentities map[string]string,
	configDigest, toolchainDigest, dependencyLockDigest, parentGenDigest string,
	ordinal int,
	cause string,
) string {
	var cmdKeys []string
	for k := range commandIdentities {
		cmdKeys = append(cmdKeys, k)
	}
	sort.Strings(cmdKeys)
	var cmdPairs []string
	for _, k := range cmdKeys {
		cmdPairs = append(cmdPairs, fmt.Sprintf("%s=%s", k, commandIdentities[k]))
	}

	payload := strings.Join([]string{
		"protocol=" + WorkGenerationProtocolVersion,
		fmt.Sprintf("ordinal=%d", ordinal),
		"cause=" + cause,
		"plan=" + planDigest,
		"git_head=" + gitHeadSHA,
		"git_tree=" + gitTreeSHA,
		"manifest=" + inputManifestDigest,
		"commands=" + strings.Join(cmdPairs, ","),
		"config=" + configDigest,
		"toolchain=" + toolchainDigest,
		"dep_lock=" + dependencyLockDigest,
		"parent=" + parentGenDigest,
	}, "\n")

	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

// ComputeEnvelopeDigest computes the canonical envelope digest binding a generation
// to a specific repository head and tree.
func ComputeEnvelopeDigest(generationDigest, finalHeadSHA, finalTreeSHA string) string {
	payload := fmt.Sprintf("envelope:v1\ngen=%s\nhead=%s\ntree=%s", generationDigest, finalHeadSHA, finalTreeSHA)
	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

// ComputeAttestationDigest computes the canonical digest of a completed WorkAttestation.
func ComputeAttestationDigest(att *WorkAttestation) string {
	if att == nil {
		return ""
	}
	clone := *att
	clone.AttestationDigest = ""
	clone.CreatedAt = 0 // Exclude timestamp from digest for determinism

	data, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
