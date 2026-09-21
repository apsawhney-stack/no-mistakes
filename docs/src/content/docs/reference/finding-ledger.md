---
title: Finding Ledger Protocol
description: The versioned, durable per-run record of every reported finding and how it was disposed of.
---

Every run keeps one durable finding ledger. It exists so a reported blocking finding cannot quietly disappear: a finding stays on the ledger, visible and blocking, until something positively disposes of it — a verified fix, or an explicit operator decision recorded with its provenance.

The ledger is per run and per step. It is the single owner of finding carry-forward; there is no second outstanding set beside the persisted round history.

## Protocol version

The ledger publishes `protocol_version`, currently `v1`. It appears in every ledger payload:

- `steps[].findings_json` on a step (the gate's own view, scoped to that step)
- `runs[].finding_ledger` and `steps[].finding_ledger` in `axi` / daemon status payloads
- `no-mistakes axi status` and `no-mistakes axi run` render it under `finding_ledger`

Consumers must treat an unfamiliar version as unrecognized rather than assuming the current shape. Pre-ledger runs and runs from older binaries report no ledger at all, and everything that reads a run keeps working without one.

### Summary shape

```json
{
  "protocol_version": "v1",
  "run_id": "01M...",
  "total_entries": 5,
  "open_count": 2,
  "pending_count": 1,
  "reconcile_count": 0,
  "closed_count": 2,
  "has_blocking": true,
  "entries": [
    {
      "id": "fn-01M...",
      "step_name": "review",
      "reported_id": "review-3",
      "severity": "error",
      "action": "ask-user",
      "file": "internal/handler.go",
      "line": 42,
      "description": "...",
      "status": "open",
      "is_blocking": true,
      "first_seen_round": 1,
      "last_observed_round": 2,
      "correcting_commit_sha": "",
      "closure_reason": ""
    }
  ]
}
```

`total_entries` and every count cover everything the run has ever recorded. `entries` deliberately carries **only the unresolved entries** — open, pending verification, and reconciliation required. That is the publishable surface: what still stands in the way of this run. Closed entries stay readable in the run's own step history and round rows, and are counted rather than re-published, so a long review loop cannot grow a status payload without bound.

## Entry identity

Each entry gets an immutable ID (`fn-…`) when it is first admitted, plus a separately persisted normalized fingerprint used only for matching.

The fingerprint is derived from the finding's own content — file, normalized description, severity, category, check identity, review scope, or its decision ID when it assesses a recorded human decision. Line numbers are excluded, so a defect that only moved lines keeps one identity. Positional labels such as `review-1`, `F1`, or `review-2` are presentation only and are never durable identity: every payload also carries `ledger_id`, and two findings from two rounds that happen to share a label stay separate entries.

Matching is deliberately conservative. An exact content match pairs first. A fingerprint match pairs only when it is unambiguous on both sides — exactly one unresolved entry and exactly one finding in the round share it. Anything else (two similar descriptions in different files, two entries with the same fingerprint, changed semantics) stays a distinct entry rather than being merged. Merging is never automatic.

## Statuses

| Status | Meaning |
| --- | --- |
| `open` | Admitted and not yet selected or disposed of. Blocking when `is_blocking` is true. |
| `pending_verification` | Selected for correction, or added by the operator as part of a fix request; it remains unresolved until a closure proof is recorded. Blocking when `is_blocking` is true. |
| `needs_reconciliation` | Imported from pre-ledger history whose outcome cannot be proven. Blocking. |
| `closed_verified` | Proven fixed by a closure proof (see below). |
| `closed_accepted` | Explicit operator acceptance or approval, with provenance and reason. |
| `closed_not_applicable` | Explicit operator decision that the finding does not apply. |
| `closed_superseded` | Explicit supersession, or a CI check finding replaced by a fresh settled observation. |

Only `closed_verified` counts as fixed in `no-mistakes stats`. An accepted exception, a not-applicable decision, a supersession, and imported reconciliation all count as reported-but-not-fixed, so a waived finding can never read as a repaired one.

## Transitions

The ledger is append-only in spirit: every transition is recorded as an event with the state before and after, the round, the step result, and any commit or session that justifies it. The entry state change and its event are committed together; if either write fails, the step fails closed instead of publishing a partial transition.

| Event | Transition |
| --- | --- |
| `admitted` | — → `open` for analyzer-reported findings, or — → `pending_verification` for operator-added fix findings |
| `reported_again` | an entry re-reported by a later round; `pending_verification` returns to `open` and its correction evidence is discarded |
| `not_rediscovered` | an unresolved entry omitted by a round; the status does not change |
| `selected_for_correction` | `open` → `pending_verification` |
| `correction_revision_recorded` | stamps the revision and the fixing session a selected correction produced; the status does not change |
| `closure_reviewed` | `pending_verification` → `closed_verified` |
| `accepted` / `not_applicable` / `superseded` | explicit disposition with provenance and reason |
| `needs_reconciliation` | `open` → `needs_reconciliation` on import |

### Union, never subtraction

Each round is unioned with every unresolved entry. A later empty or partial round cannot close an earlier blocking finding: an omission records `not_rediscovered` and changes nothing.

Selecting a subset for correction moves exactly those entries to `pending_verification`. Every unselected entry stays `open` and unresolved. There is no "empty selection means everything" rule — an implicit select-all would silently widen what one fix response authorizes.

### Verified closure

`closed_verified` is a proof, not a conclusion. All of the following must hold:

1. A correcting revision was recorded when the correction ran.
2. The turn that certifies closure is a different session from the one that applied the correction. A session-free turn counts as independent: it is a fresh invocation by construction.
3. The closure review is of that correcting revision, not some other head.
4. Step-specific positive evidence:
   - **review** — a fresh review covered the corrected file completely and did not re-report the defect in it, or returned exactly one `satisfied` assessment with nonblank evidence for the finding's recorded decision.
   - **test** — the live-validation verdict is `go` on a zero exit status, and the tested head is the correcting revision when one is reported.
   - **deterministic steps** (lint, document, gates) — the step re-ran clean on the corrected revision: exit zero with nothing re-reported.

A rejected fix cannot keep vouching for itself: if a rereview reports the defect again, the entry returns to `open` and its recorded revision, fixing session, and evidence are cleared.

### CI check findings

CI check findings are live measurements, not persistent claims about code, so the CI step owns their set: a fresh **settled** clean observation supersedes the check-anchored entries it no longer reports. That supersession is recorded as `closed_superseded` with the observed head and provenance `ci_settled_observation`, which keeps a red that simply stopped reproducing distinguishable from a proven repair. A non-clean or unresolved round supersedes nothing, and a review-bot comment is not a check observation — it stays open until a human decides.

### Step-owned findings

Some findings are synthesized by their own step from live state on every round rather than reported about the change: a Test budget cut, a failing configured `commands.test`, a protected-path refusal, a CI fix-agent budget cut, or the Test step's informational note that it wrote a new test file. The parked findings are operator decisions rather than repair targets, and the informational new-test-file note is a live-state report; each clears by resolving or re-measuring that live state rather than by repairing a reported defect.

They keep their existing owner and are deliberately **not** admitted to the ledger. They are still carried on the round's payload, so the gate, approval-refusal checks, and status readers see them, but they are absent from every ledger count. Admitting them would create entries nothing could ever close, because no fix round selects them.

## Terminal acceptance

A run is refused a clean completion for any unresolved ledger state that would otherwise certify the run without proof:

- any blocking entry is `open`
- any entry is `pending_verification` without closure evidence
- any entry `needs_reconciliation`

The executor also parks a step for the entries terminal acceptance would refuse: an entry in **pending verification**, **reconciliation required**, or **open and blocking** reaches a gate for a decision instead of dead-ending the run at terminal acceptance.

A merely `open` entry keeps the semantics it had before the ledger existed. A blocking one parks its step through the ledger summary and the ordinary blocking or ask-user finding checks, while an explicitly non-blocking informational no-op note does not park its step at all; it stays visible in the ledger until something disposes of it.

## Legacy runs

The ledger never rewrites history.

- **Completed runs** are never imported. Their records were written under pre-ledger semantics that carry no acceptance provenance this protocol can honestly restate, and importing them would either invent a disposition or restate old fixed counts as unfixed.
- **Active pre-ledger runs** are imported the first time the pipeline adopts the run (when its ledger is created). Every available persisted finding and round record is imported under step- and round-aware identity. A finding reported in an earlier round but absent from the run's latest round becomes `needs_reconciliation`: the pre-ledger engine could not say why it stopped being reported, so the import says exactly that instead of guessing "fixed". Everything else stays `open` and is disposed of through the ordinary round, selection, and closure path.
- Import is **idempotent and atomic**. One transaction writes the entries, the events, and a per-run migration marker; the marker — not the presence of imported entries — decides whether a run is already migrated, so an interrupted attempt is completed on retry rather than mistaken for a complete history.

## Privacy and retention

The ledger stores the finding text and file paths the run already recorded, in the same database, under the same run access, privacy, and retention rules. It adds no prompt, diff, transcript, or new telemetry, and publishes nothing remotely: `axi`/`status` reads are local read-only surfaces, and only the bounded counts already reported on the terminal `run finished` event leave the machine.

Reconciliation-required entries can be resolved by an explicit operator decision at their gate; that closure is recorded as an operator disposition with `:reconciled` provenance and never as `closed_verified`.
