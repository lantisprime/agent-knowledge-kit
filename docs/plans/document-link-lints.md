# Document links and release lints

Status: **approved post-v1 implementation slice** (operator decision
2026-08-08).

This slice adds immutable, version-specific document links and closes the
draft-reference and dangling-supersession release-policy gaps documented in
`knowledge-server.md`. It does not add collections, lifecycle configuration,
promotion provenance, automated claim detection, or content extraction.

## Contract

Every `DocSave` accepts an optional `links` array. Omission is equivalent to an
empty array, preserving compatibility with v1 clients. Each item has this wire
shape:

```json
{
  "relation": "reference",
  "collection": "docs",
  "family_id": "recovery",
  "version": 3
}
```

`relation` is exactly `reference` or `supersedes`. Collection and family use
the existing path-safe identifier grammar. Version is a positive integer.
Duplicate links within one saved version and a link to the saved version
itself are invalid input.

Links belong to one immutable source version and preserve request order.
Targets deliberately have no foreign key: authors may save drafts containing
forward references, and release linting—not save-time existence checking—is
the policy gate.

`GET /api/docs/{collection}/{family}[?version=N]` returns `links` as an array,
including `[]` for documents saved by older clients. List and history metadata
remain unchanged.

## Release lints

For each active source document in a release-bearing collection, in release
candidate order and then link order:

1. A `reference` target must exist as the exact named version, have status
   `active`, and belong to a release-bearing collection. This blocks missing,
   draft, superseded, and non-release targets; the draft case is the named
   draft-reference policy gap.
2. A `supersedes` target must exist as the exact named version and have status
   `superseded`. A missing target is the named dangling-supersession gap;
   draft or active targets are also invalid supersession claims.

The first failure returns the existing typed `ErrLint` path. Preview rolls
back without recording anything. Cut records or refreshes one policy conflict
against the source document and returns its `conflict_id`. A later successful
cut resolves open policy conflicts under the existing release-clearing
semantics.

Links are server governance metadata: they are not prepended to archive bodies
and do not change the body-derived `content_hash`. The cut therefore always
re-evaluates link lints inside the same transaction, even when the caller's
expected content hash still matches. `release_docs` pins the selected source
version in the immutable release record.

## UI

The editor and conflict merge form expose links as a JSON array. Shape parsing
is local and fail-closed before a request is sent; the server remains the trust
boundary and completes identifier, duplicate, and self-link validation. Link
metadata participates in the merge view's metadata comparison.

## Writable set

- `knowledge-server/store/store.go`
- `knowledge-server/store/store_test.go`
- `knowledge-server/api_test.go`
- `knowledge-server/ui/{index.html,app.js,lib.mjs,lib_test.mjs}`
- `README.md`, `CLAUDE.md`, `docs/architecture.md`, `docs/REPO_MAP.md`
- `docs/plans/{knowledge-server.md,document-link-lints.md}`
- `schema/frontmatter.md`

No subscriber, adapter, archive, authentication, fleet, deployment, or
consumer-repository behavior changes in this slice.

## Verification

Negative tests land before implementation and prove the schema, wire types,
UI parsing, and release-lint behavior are absent. Completion requires:

```sh
(cd knowledge-server && go test -race ./...)
(cd knowledge-server && node --test ui/lib_test.mjs)
sh -n tests/run.sh && sh tests/run.sh
shellcheck -x adapters/lib/kernel-path.sh adapters/claude/install.sh \
  adapters/codex/update-agents-md.sh adapters/pi/run.sh tests/run.sh
git diff --check
```
