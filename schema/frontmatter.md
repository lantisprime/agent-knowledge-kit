# Corpus document schema

Every corpus document is markdown with YAML frontmatter:

```yaml
---
title: Cluster test-sandbox contract
status: active            # active | superseded | draft
supersedes: null          # slug of the doc this replaces, if any
owner: ops                # who answers for this doc's correctness
audience: agent           # agent | human | both
tier: B                   # A (kernel-included) | B (trigger-loaded) | C (query-only)
verify: "kubectl get ns"  # optional: falsifiable check for the doc's key claim
generated-from: null      # optional: source-of-truth ref for factual tables
                          #   (inventory system, CMDB, IaC output) — facts are
                          #   generated, never hand-forked
triggers: ["deploy", "kubectl", "ssh"]   # optional: Tier B load hints
---
```

Rules:

1. **One claim, one home.** A fact lives in its source of truth and is
   `generated-from` everywhere else. A procedure lives in exactly one doc.
2. **Supersede in place.** Never publish a corrected doc alongside the old
   one as a peer. Flip the old doc to `status: superseded`, point
   `supersedes:` from the new one, and let git history keep the past.
3. **Kernel changes are PR-only** and bounded by the kernel token cap. If a
   kernel edit pushes past the cap, something else must leave — that
   argument happens in the PR, not in the file.
4. **`verify:` beats prose.** For capability claims ("harness X loads file
   Y"), include the one-line check that proves it. Reviewers run it; agents
   can too.
5. **Drafts don't ship.** `status: draft` docs are excluded from the synced
   bundle by the sync tooling.
