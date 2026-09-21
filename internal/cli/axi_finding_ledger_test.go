package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunObjectProjectionPublishesTheVersionedLedger pins the protocol surface:
// a status read carries the ledger's protocol version and every blocking count,
// including reconciliation-required entries, so a consumer never has to parse a
// step payload to learn that a run is waiting on a decision.
func TestRunObjectProjectionPublishesTheVersionedLedger(t *testing.T) {
	rv := runView{
		ID:     "run-1",
		Branch: "feature",
		Status: string(types.RunRunning),
		FindingLedger: &types.FindingLedgerSummary{
			ProtocolVersion: types.FindingLedgerProtocolVersion,
			RunID:           "run-1",
			TotalEntries:    5,
			OpenCount:       2,
			PendingCount:    1,
			ReconcileCount:  1,
			ClosedCount:     1,
			HasBlocking:     true,
		},
	}
	out := axiDoc(runObjectField(rv))
	for _, want := range []string{"finding_ledger:", "version: v1", "total: 5", "open: 2", "pending: 1", "reconcile: 1", "closed: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("run projection missing %q:\n%s", want, out)
		}
	}
}

// TestRunObjectProjectionOmitsAnAbsentLedger pins the compatibility half: a
// pre-ledger run (no ledger) projects exactly as it did before, with no empty
// ledger object invented for it.
func TestRunObjectProjectionOmitsAnAbsentLedger(t *testing.T) {
	rv := runView{ID: "run-1", Branch: "feature", Status: string(types.RunRunning)}
	out := axiDoc(runObjectField(rv))
	if strings.Contains(out, "finding_ledger") {
		t.Fatalf("absent ledger was projected:\n%s", out)
	}
}
