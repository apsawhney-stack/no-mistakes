---
title: Work Generation Protocol
description: Deterministic content generations, phase-scoped write sets, proof invalidation, and final work attestations.
---

Every run validates a specific, immutable content generation. The work generation protocol binds all validation evidence (reviews, tests, lints, and CI checks) to deterministic tree and manifest digests. When source inputs mutate, dependent proofs are transitively invalidated, moving the run to generation $N+1$, while narrative-only documentation changes update the repository envelope without invalidating code validation.

## Protocol version

The work generation protocol publishes `protocol_version`, currently `v1`. It appears in:

- `runs[].work_generation` in `axi` / daemon status payloads
- `no-mistakes axi status` and `no-mistakes axi run` under `work_generation`
- PR descriptions as a cryptographically verifiable attestation block

### Summary shape

```json
{
  "version": "v1",
  "current_generation": 1,
  "generation_digest": "3fa910...",
  "plan_id": "plan-conservative-default",
  "write_set_verdict": "clean",
  "has_attestation": true,
  "phase_results": [
    {
      "id": "res-01M...-intent-1",
      "phase": "intent",
      "status": "passed",
      "applicable": true
    },
    {
      "id": "res-01M...-review-1",
      "phase": "review",
      "status": "passed",
      "applicable": true
    }
  ]
}
```

## Generation Identity & Hashing

A work generation represents an immutable snapshot of inputs, validation plan, and environment digests:

1. **Plan Digest**: Deterministic SHA-256 over Plan ID, version, required phases, selected input patterns, commands, and excluded protected paths.
2. **Input Manifest Digest**: Merkle tree of selected input files, recording relative path, file mode, node type, and content SHA-256 (rejecting external/unauthorized symlinks).
3. **Generation Digest**: Hash combining the plan digest, git head SHA, git tree SHA, input manifest digest, command identities, dependency lockfile digest, toolchain identity, parent generation digest, ordinal, and mutation cause.

## Phase-Scoped Write Sets

To ensure pipeline integrity, each phase operates under strict write-set boundaries:

- **Read-Only Phases**: Phases such as `review` (outside fix mode), `test` (without test-fix), and `intent` are strictly read-only with respect to tracked files.
- **Phase-Specific Writers**:
  - `lint`: May modify files matching configured source patterns (e.g. `**/*.go`).
  - `document`: May only modify documentation files (`docs/**`, `*.md`, `README*`, `LICENSE*`, `CONTRIBUTING*`, `CHANGELOG*`).
  - Fix turns (`fixing=true` in `review`, `ci`, or `test`): Permitted to modify files matching the phase's allowed write patterns or selected fix targets.
- **Engine-Protected Exclusions**: Tracked files that are *never* permitted to be modified by agent turns include:
  - Internal engine files (`.no-mistakes/**`, findings dumps, evidence logs)
  - Git repository metadata (`.git/**`)
  - Secrets and credentials (`**/*.pem`, `**/*.key`, `**/*token*`, `**/*secret*`, `**/*credential*`)
  - Build/CI definitions unless explicitly configured

### Unauthorized Write Refusals

If an agent or step makes unauthorized modifications outside its permitted phase write set or touches protected paths:

1. The worktree is restored to its pre-phase state.
2. An unauthorized write finding (`FindingIDUnauthorizedWriteRefusal`: `unauthorized-write-refusal`) is emitted.
3. The run parks at the approval gate as an `ask-user` error, requiring human operator inspection and decision before proceeding.

## Mutation Lifecycle ($N \to N+1$)

When an authorized fix or code generation step modifies files matching selected inputs:

1. A new generation $N+1$ is allocated with an incremented ordinal and a reference to parent generation $N$.
2. Dependent proof results from earlier phases that relied on the mutated files are marked `applicable = false` and `status = stale`.
3. The pipeline resets execution back to the earliest invalidated phase (typically `review`).
4. **Finding Ledger Reconciliation**: Any finding in the Finding Ledger that was previously marked `closed_verified` is reopened if the mutated files intersect the finding's file scope, ensuring fixes are re-verified on the new generation.

### Selected Content vs. Final Envelope

A key distinction is made between selected code inputs and repository envelope changes:

- **Code Changes**: Modifying application logic, tests, or build scripts increments the generation ($N \to N+1$) and requires re-validation.
- **Narrative-Only Changes**: Modifications exclusively affecting narrative documentation (`docs/**`, `*.md`, `README*`, `LICENSE*`, `CONTRIBUTING*`, `CHANGELOG*`) preserve Generation $N$. Instead of rewinding the validation pipeline, a new **Final Envelope** digest is computed over the new head and tree SHA, allowing doc touch-ups to land without invalidating green test and review proofs.

## Terminal Acceptance & Attestation

Before a run can transition to `types.RunCompleted`:

1. All phases required by the validation plan must have a recorded result for the active generation with status `passed` (or `not_applicable` with an explicit plan reason).
2. No unauthorized writes or unresolved blocking findings may exist on the ledger.
3. CI checks must match the exact final git head SHA.
4. An immutable **Work Attestation** is computed and recorded in the database, locking the plan digest, generation digest, envelope digest, phase summaries, and evidence references.

The attestation digest is published into the PR summary comment, providing a verifiable proof of correct pipeline execution.

## Active Run Migration

Pre-Increment F runs in progress during daemon upgrade are migrated idempotently:

- Completed historical runs are preserved without modification.
- Active legacy runs have their pre-existing unprovenanced phase results marked stale/unknown, are registered in `work_generation_migrations`, and initialize Generation 0 on next step execution.
