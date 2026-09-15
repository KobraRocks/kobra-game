<!--
Keep the description small and single-purpose. Delete any section that does not
apply, but do not delete the gate list: reviewers need to know what you ran.
-->

## What

<!-- One or two sentences. What changes, from the user's point of view. -->

## Why

<!-- The bug, the spec rule, or the problem this solves. Link the issue if there is one. -->

Fixes #

## Which gates did you run?

<!-- Paste the result line, or say which ones you skipped and why. -->

- [ ] `make -C launcher fmt vet test race`
- [ ] `make -C packaging check race`
- [ ] `make -C launcher faultinject` — required for storage, updater or request-path changes
- [ ] `make -C testgame verify` — required for packager changes
- [ ] `make -C testgame e2e`
- [ ] `bash .e2e/run.sh`
- [ ] `make -C launcher cross` — required for platform-specific files

## Does this change a shipped contract?

<!-- Tick every one that applies. These break other people's games, so they need
     a version bump or an explicitly versioned addition. -->

- [ ] No shipped contract changes
- [ ] Data API shape (`architecture/schemas/data-api.schema.json`)
- [ ] Launcher config schema
- [ ] A manifest schema (engine, asset, mod, release, save)
- [ ] An event name, exit code, or the on-disk layout of `data/`
- [ ] Required behaviour described in `architecture/*.md` — **updated in this PR**

## Checklist

- [ ] The change is covered by a test, and a bug fix has a regression test named
      after the failure.
- [ ] The gates above pass, and I ran the ones the table marks required for this area.
- [ ] If behaviour described in the specs changed, the spec is updated here.
- [ ] No user-facing message contains a path, username, token or stack trace.
- [ ] If a dependency was added or bumped, `THIRD_PARTY_NOTICES.md` is updated.
- [ ] I did not add a test-only override that bypasses production behaviour.

## What I did not do

<!--
Optional but useful: deliberate omissions, known limitations, or follow-up work
you are leaving for a later PR. "Nothing" is a fine answer.
-->
