package types

import (
	"testing"
)

func TestLedgerStatusHelpers(t *testing.T) {
	unresolved := []string{
		FindingLedgerStatusOpen,
		FindingLedgerStatusPendingVerification,
		FindingLedgerStatusNeedsReconciliation,
	}
	for _, s := range unresolved {
		if !IsUnresolvedLedgerStatus(s) {
			t.Errorf("IsUnresolvedLedgerStatus(%q) = false, want true", s)
		}
		if IsClosedLedgerStatus(s) {
			t.Errorf("IsClosedLedgerStatus(%q) = true, want false", s)
		}
	}

	closed := []string{
		FindingLedgerStatusClosedVerified,
		FindingLedgerStatusClosedAccepted,
		FindingLedgerStatusClosedNotApplicable,
		FindingLedgerStatusClosedSuperseded,
	}
	for _, s := range closed {
		if !IsClosedLedgerStatus(s) {
			t.Errorf("IsClosedLedgerStatus(%q) = false, want true", s)
		}
		if IsUnresolvedLedgerStatus(s) {
			t.Errorf("IsUnresolvedLedgerStatus(%q) = true, want false", s)
		}
	}
}

func TestIsBlockingFinding(t *testing.T) {
	tests := []struct {
		finding Finding
		want    bool
	}{
		{finding: Finding{Severity: FindingSeverityError, Action: ActionAutoFix}, want: true},
		{finding: Finding{Severity: FindingSeverityWarning, Action: ActionNoOp}, want: true},
		{finding: Finding{Severity: FindingSeverityInfo, Action: ActionAskUser}, want: true},
		{finding: Finding{Severity: FindingSeverityInfo, Action: ActionNoOp}, want: false},
		{finding: Finding{Severity: FindingSeverityInfo, Action: ""}, want: true}, // ActionOrDefault defaults to ask-user
	}
	for i, tc := range tests {
		got := IsBlockingFinding(tc.finding)
		if got != tc.want {
			t.Errorf("test[%d] IsBlockingFinding(%+v) = %v, want %v", i, tc.finding, got, tc.want)
		}
	}
}

func TestNormalizeFingerprint_LineIndependent(t *testing.T) {
	f1 := Finding{
		ID:          "review-1",
		File:        "pkg/server.go",
		Line:        42,
		Severity:    "ERROR",
		Description: "nil pointer dereference when conn is closed",
		Action:      "auto-fix",
	}
	f2 := Finding{
		ID:          "review-99",
		File:        "./pkg/server.go",
		Line:        105,
		Severity:    "error",
		Description: "nil pointer  dereference   when conn is closed",
		Action:      "ask-user",
	}
	fp1 := NormalizeFingerprint(f1)
	fp2 := NormalizeFingerprint(f2)
	if fp1 != fp2 {
		t.Fatalf("NormalizeFingerprint mismatch:\nfp1=%s\nfp2=%s", fp1, fp2)
	}

	// Different file should not match
	f3 := f1
	f3.File = "pkg/client.go"
	if NormalizeFingerprint(f3) == fp1 {
		t.Fatalf("NormalizeFingerprint should differ for different files")
	}

	// Different description should not match
	f4 := f1
	f4.Description = "completely different defect"
	if NormalizeFingerprint(f4) == fp1 {
		t.Fatalf("NormalizeFingerprint should differ for different descriptions")
	}
}

func TestFindingsJSON_RoundtripsLedgerAndLedgerID(t *testing.T) {
	original := Findings{
		Summary: "1 open issue",
		Items: []Finding{
			{
				ID:          "review-1",
				LedgerID:    "fn-01HQTEST1234567890",
				Severity:    "error",
				File:        "main.go",
				Line:        10,
				Description: "memory leak",
				Action:      "auto-fix",
			},
		},
		Ledger: &FindingLedgerSummary{
			ProtocolVersion: FindingLedgerProtocolVersion,
			RunID:           "run-123",
			TotalEntries:    1,
			OpenCount:       1,
			HasBlocking:     true,
			Entries: []FindingLedgerEntrySummary{
				{
					ID:             "fn-01HQTEST1234567890",
					StepName:       StepReview,
					ReportedID:     "review-1",
					Severity:       "error",
					Action:         "auto-fix",
					File:           "main.go",
					Line:           10,
					Description:    "memory leak",
					Status:         FindingLedgerStatusOpen,
					IsBlocking:     true,
					FirstSeenRound: 1,
				},
			},
		},
	}

	raw, err := MarshalFindingsJSON(original)
	if err != nil {
		t.Fatalf("MarshalFindingsJSON: %v", err)
	}

	parsed, err := ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("ParseFindingsJSON: %v", err)
	}

	if len(parsed.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(parsed.Items))
	}
	if parsed.Items[0].LedgerID != "fn-01HQTEST1234567890" {
		t.Errorf("Items[0].LedgerID = %q, want fn-01HQTEST1234567890", parsed.Items[0].LedgerID)
	}
	if parsed.Ledger == nil {
		t.Fatalf("parsed.Ledger is nil")
	}
	if parsed.Ledger.ProtocolVersion != FindingLedgerProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", parsed.Ledger.ProtocolVersion, FindingLedgerProtocolVersion)
	}
	if parsed.Ledger.TotalEntries != 1 || parsed.Ledger.OpenCount != 1 || !parsed.Ledger.HasBlocking {
		t.Errorf("unexpected ledger counts: %+v", parsed.Ledger)
	}
	if len(parsed.Ledger.Entries) != 1 || parsed.Ledger.Entries[0].ID != "fn-01HQTEST1234567890" {
		t.Errorf("unexpected ledger entries: %+v", parsed.Ledger.Entries)
	}
}
