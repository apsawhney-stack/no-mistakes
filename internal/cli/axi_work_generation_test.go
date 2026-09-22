package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunObjectProjectionPublishesWorkGeneration pins the work generation CLI/AXI surface.
func TestRunObjectProjectionPublishesWorkGeneration(t *testing.T) {
	rv := runView{
		ID:     "run-gen-1",
		Branch: "feature",
		Status: string(types.RunRunning),
		WorkGeneration: &types.WorkGenerationSummary{
			ProtocolVersion:          types.WorkGenerationProtocolVersion,
			RunID:                    "run-gen-1",
			CurrentGenerationID:      "gen-run-gen-1-2",
			CurrentGenerationOrdinal: 2,
			CurrentGenerationDigest:  "abc123gen",
			PlanID:                   "conservative-default",
			WriteSetVerdict:          "passed",
			AttestationID:            "att-run-gen-1",
			AttestationDigest:        "attdigest123",
			WorkAttestation: &types.WorkAttestation{
				ID:                  "att-run-gen-1",
				ProtocolVersion:     types.WorkGenerationProtocolVersion,
				RunID:               "run-gen-1",
				GenerationID:        "gen-run-gen-1-2",
				GenerationDigest:    "abc123gen",
				PlanID:              "conservative-default",
				PlanDigest:          "plandigest123",
				FinalHeadSHA:        "head123",
				FinalEnvelopeDigest: "envdigest123",
				AttestationDigest:   "attdigest123",
			},
		},
	}
	out := axiDoc(runObjectField(rv))
	for _, want := range []string{
		"work_generation:",
		"version: v1",
		"current_generation: 2",
		"generation_digest: abc123gen",
		"plan_id: conservative-default",
		"write_set_verdict: passed",
		"has_attestation: true",
		"attestation_digest: attdigest123",
		"work_attestation:",
		"\\\"attestation_digest\\\":\\\"attdigest123\\\"",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("run projection missing %q:\n%s", want, out)
		}
	}
}

// TestRunObjectProjectionOmitsAbsentWorkGeneration pins compatibility when no work generation exists.
func TestRunObjectProjectionOmitsAbsentWorkGeneration(t *testing.T) {
	rv := runView{ID: "run-1", Branch: "feature", Status: string(types.RunRunning)}
	out := axiDoc(runObjectField(rv))
	if strings.Contains(out, "work_generation") {
		t.Fatalf("absent work generation was projected:\n%s", out)
	}
}
