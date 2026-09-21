package types

import (
	"testing"
)

func TestComputePlanDigest_Deterministic(t *testing.T) {
	plan1 := &ValidationPlan{
		Version:        "v1",
		PlanID:         "test-plan",
		SelectedInputs: []string{"**/*.go", "go.mod", "go.sum"},
		RequiredPhases: []StepName{StepReview, StepTest, StepLint, StepDocument, StepCI},
		Commands: map[string]string{
			"test": "go test ./...",
			"lint": "make lint",
		},
		PhaseWriteSets: map[string][]string{
			"document": {"docs/**", "*.md"},
		},
	}

	plan2 := &ValidationPlan{
		Version:        "v1",
		PlanID:         "test-plan",
		SelectedInputs: []string{"go.sum", "**/*.go", "go.mod"},                          // different order
		RequiredPhases: []StepName{StepCI, StepDocument, StepLint, StepReview, StepTest}, // different order
		Commands: map[string]string{
			"lint": "make lint",
			"test": "go test ./...",
		},
		PhaseWriteSets: map[string][]string{
			"document": {"docs/**", "*.md"},
		},
	}

	d1 := ComputePlanDigest(plan1)
	d2 := ComputePlanDigest(plan2)

	if d1 == "" {
		t.Fatal("expected non-empty plan digest")
	}
	if d1 != d2 {
		t.Fatalf("expected plan digests to match despite different input order, got %s vs %s", d1, d2)
	}
}

func TestComputeInputManifestDigest_Deterministic(t *testing.T) {
	entries1 := []InputManifestEntry{
		{Path: "b/file.go", Mode: 0644, Type: "regular", Digest: "hash-b", Size: 200},
		{Path: "a/file.go", Mode: 0644, Type: "regular", Digest: "hash-a", Size: 100},
	}
	entries2 := []InputManifestEntry{
		{Path: "a/file.go", Mode: 0644, Type: "regular", Digest: "hash-a", Size: 100},
		{Path: "b/file.go", Mode: 0644, Type: "regular", Digest: "hash-b", Size: 200},
	}

	d1 := ComputeInputManifestDigest(entries1)
	d2 := ComputeInputManifestDigest(entries2)

	if d1 == "" {
		t.Fatal("expected non-empty manifest digest")
	}
	if d1 != d2 {
		t.Fatalf("expected identical manifest digest for reordered entries, got %s vs %s", d1, d2)
	}

	// Change mode
	entries3 := []InputManifestEntry{
		{Path: "a/file.go", Mode: 0755, Type: "regular", Digest: "hash-a", Size: 100}, // executable mode
		{Path: "b/file.go", Mode: 0644, Type: "regular", Digest: "hash-b", Size: 200},
	}
	d3 := ComputeInputManifestDigest(entries3)
	if d3 == d1 {
		t.Fatal("expected different manifest digest when file mode changes")
	}

	// Change type
	entries4 := []InputManifestEntry{
		{Path: "a/file.go", Mode: 0644, Type: "symlink", Digest: "hash-a", Size: 100},
		{Path: "b/file.go", Mode: 0644, Type: "regular", Digest: "hash-b", Size: 200},
	}
	d4 := ComputeInputManifestDigest(entries4)
	if d4 == d1 {
		t.Fatal("expected different manifest digest when file type changes")
	}
}

func TestComputeGenerationDigest_Sensitivity(t *testing.T) {
	baseDigest := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-1", "tree-sha-1", "manifest-hash-1",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-1", "",
		0, GenerationCauseInitial,
	)

	// Different head
	diffHead := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-2", "tree-sha-1", "manifest-hash-1",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-1", "",
		0, GenerationCauseInitial,
	)
	if diffHead == baseDigest {
		t.Fatal("digest should change when git head changes")
	}

	// Different tree
	diffTree := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-1", "tree-sha-2", "manifest-hash-1",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-1", "",
		0, GenerationCauseInitial,
	)
	if diffTree == baseDigest {
		t.Fatal("digest should change when git tree changes")
	}

	// Different manifest
	diffManifest := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-1", "tree-sha-1", "manifest-hash-2",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-1", "",
		0, GenerationCauseInitial,
	)
	if diffManifest == baseDigest {
		t.Fatal("digest should change when manifest changes")
	}

	// Different lock file
	diffLock := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-1", "tree-sha-1", "manifest-hash-1",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-2", "",
		0, GenerationCauseInitial,
	)
	if diffLock == baseDigest {
		t.Fatal("digest should change when dependency lock changes")
	}

	// Different ordinal
	diffOrdinal := ComputeGenerationDigest(
		"plan-hash-1", "head-sha-1", "tree-sha-1", "manifest-hash-1",
		map[string]string{"test": "go test"},
		"config-hash-1", "toolchain-1", "lock-1", "parent-1",
		1, GenerationCauseSourceMutation,
	)
	if diffOrdinal == baseDigest {
		t.Fatal("digest should change when ordinal changes")
	}
}

func TestComputeEnvelopeDigest(t *testing.T) {
	e1 := ComputeEnvelopeDigest("gen-digest-1", "head-1", "tree-1")
	e2 := ComputeEnvelopeDigest("gen-digest-1", "head-1", "tree-1")
	if e1 != e2 {
		t.Fatalf("expected identical envelope digest, got %s vs %s", e1, e2)
	}

	// Narrative change moves head and tree
	e3 := ComputeEnvelopeDigest("gen-digest-1", "head-2", "tree-2")
	if e3 == e1 {
		t.Fatal("expected envelope digest to reflect updated head and tree")
	}
}

func TestComputeAttestationDigest(t *testing.T) {
	att := &WorkAttestation{
		ProtocolVersion:     WorkGenerationProtocolVersion,
		RunID:               "run-1",
		GenerationID:        "gen-1",
		GenerationDigest:    "gen-hash-1",
		GenerationOrdinal:   0,
		PlanID:              "plan-1",
		PlanDigest:          "plan-hash-1",
		FinalHeadSHA:        "head-1",
		FinalTreeSHA:        "tree-1",
		FinalEnvelopeDigest: "env-1",
		CIHeadSHA:           "head-1",
		CreatedAt:           1234567890,
	}

	d1 := ComputeAttestationDigest(att)
	if d1 == "" {
		t.Fatal("expected non-empty attestation digest")
	}

	// CreatedAt change should not affect digest
	att.CreatedAt = 9999999999
	d2 := ComputeAttestationDigest(att)
	if d1 != d2 {
		t.Fatalf("expected attestation digest to exclude volatile timestamp: %s vs %s", d1, d2)
	}
}

func TestStepOwnedFindingIDs_IncludesUnauthorizedWrite(t *testing.T) {
	finding := Finding{
		ID:       FindingIDUnauthorizedWriteRefusal,
		Severity: FindingSeverityError,
		Action:   ActionAskUser,
	}
	if !IsStepOwnedFinding(finding) {
		t.Fatalf("expected FindingIDUnauthorizedWriteRefusal to be classified as step-owned")
	}
}
