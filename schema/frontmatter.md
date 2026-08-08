# Portable corpus metadata reference

The knowledge server stores document metadata in SQLite and accepts writes
only through its UI/API. It does **not** import Markdown frontmatter. This reference
defines the portable shape used when exporting, reviewing, or proposing a
document outside the server; the HTTP contract in
`docs/plans/knowledge-server.md` and the link contract in
`docs/plans/document-link-lints.md` remain authoritative.

```yaml
---
collection: docs            # kernel | docs in v1
family-id: test-sandbox     # stable family identifier
version: 3                  # server-assigned; omit on a new save
title: Test-sandbox contract
status: active              # draft | active | superseded
owner: ops
audience: agent
tier: B
triggers: ["deploy", "test"]
links:
  - relation: reference       # reference | supersedes
    collection: docs
    family-id: recovery
    version: 2
editor: operator            # server-recorded identity
created-at: 2026-08-08T00:00:00Z
---
```

The Markdown below the frontmatter is the document `body`. The subscriber's
release archive contains body bytes at the manifest path; it does not prepend
this metadata.

## Enforced server rules

1. Every save inserts a new immutable version. The server assigns `version`,
   `editor`, and `created-at`.
2. At most one active version exists per `(collection, family-id)`.
3. Draft and superseded versions remain in history but do not enter releases.
4. The `kernel` and `docs` collections enter v1 releases. Query-only future
   collections do not exist yet.
5. A release is blocked when an active kernel body exceeds 2,000 words or
   24,576 bytes.
6. Collection and family identifiers use the server's narrow validated
   character set because they become archive paths.
7. Trigger strings may not be empty or contain a comma in v1.
8. Links belong to the immutable source version and name an exact target
   collection, family, and positive version. Active references must resolve to
   active release-bearing targets; supersession targets must exist and already
   be superseded. Drafts may carry forward links until release.

## Not represented

`verify`, `generated-from`, write-path policy, lifecycle policy, and promotion
provenance are not stored fields. Supersession is represented by a
`relation: supersedes` link rather than a separate top-level field. Adding the
remaining fields requires an additive schema/API slice; prose or frontmatter
alone does not make them enforced behavior.
