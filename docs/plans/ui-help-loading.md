# Curation UI guidance and asynchronous-state hardening

Status: **frozen implementation slice** (2026-08-08).

This slice improves operator onboarding and prevents asynchronous document,
save, and history responses from presenting or mutating the wrong document.
It does not change the API, store, subscriber, adapters, release semantics,
authentication, deployment, or consumer repositories.

## Writable set

- `README.md`
- `knowledge-server/ui/app.js`
- `knowledge-server/ui/index.html`
- `knowledge-server/ui/style.css`
- `knowledge-server/ui_test.go`
- `docs/plans/ui-help-loading.md`

Everything else is read-only. In particular, `.claude/` is pre-existing local
state and must not be staged or committed.

## Behavior contracts

- **UX-1 — discoverability:** the Documents view explains collections, family
  ids, immutable versions/statuses, metadata, and exact-version links. Editor
  fields expose concise accessible guidance.
- **UX-2 — editor load gate:** editor fields, History, and Reload start
  disabled. A 200 latest-version response or expected new-document 404 enables
  editing; every other response leaves editing disabled and enables Reload.
- **UX-3 — editor request identity:** late document-load and save responses
  must not overwrite or mutate a subsequently opened editor. Duplicate save
  submissions for one editor are ignored while its PUT is pending.
- **UX-4 — history context:** history rendered for one document must never be
  shown beneath another document's path. A failed refresh may preserve prior
  history only when it belongs to the same collection/family.
- **UX-5 — history request identity:** list, version-view, and diff responses
  apply only when both the editor object and monotonically increasing history
  request generation still match.
- **UX-6 — recoverable history errors:** Back to editor remains available when
  the initial history fetch fails. List/view/diff errors use the dedicated live
  error region and do not erase a successful diff or fallback rendering.
- **UX-7 — scope/public boundary:** no private environment data, new
  dependencies, inline event handlers, unsafe HTML sinks, or unrelated
  architecture changes.

## Style anchors

- Mirror existing DOM helpers and request-identity guards in
  `knowledge-server/ui/app.js`; dynamic values remain text-only.
- Mirror the current Apple-inspired tokens, responsive rules, and form styles
  in `knowledge-server/ui/style.css`.
- Keep browser-free UI wiring checks in `knowledge-server/ui_test.go` narrow
  and honest: they pin served source/markup contracts, while the recorded
  manual browser checklist supplies runtime DOM and accessibility evidence.

## Build and verification order

1. Add or refine failing source-contract checks for UX-2 through UX-6.
   Verify: the focused Go test fails for each absent guard before source edits.
2. Implement the minimum markup and JavaScript changes.
   Verify: `node --check knowledge-server/ui/app.js` and the focused Go test.
3. Verify the full slice:

   ```sh
   (cd knowledge-server && GOCACHE=/tmp/agent-knowledge-kit-go-cache go test -race -count=1 ./...)
   (cd knowledge-server && node --check ui/app.js && node --test ui/lib_test.mjs)
   sh -n tests/run.sh && sh tests/run.sh
   shellcheck -x adapters/lib/kernel-path.sh adapters/claude/install.sh \
     adapters/codex/update-agents-md.sh adapters/pi/run.sh tests/run.sh
   git diff --check
   ```

4. Review the frozen diff against UX-1 through UX-7. Every finding receives
   `ACCEPT`, `ACCEPT-WITH-MOD`, `REJECT`, `DEFER`, or `NEEDS-EVIDENCE`; any
   held finding is folded and re-reviewed before commit.

## Seat hard rules

- Builder may edit only the writable set and must not commit, stash, push,
  create a PR, or write episodic memory.
- Reviewers are read-only and cite contract ids plus file/line evidence.
- No seat touches consumer repositories, homelab services, credentials,
  recovery artifacts, worktrees, or `.claude/`.
