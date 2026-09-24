---
name: Story
about: A scoped unit of work that is neither a bug report nor a feature request (docs, refactor, tooling, hardening)
title: '[STORY] '
assignees: ''
---

<!-- The sections below are the minimum an agent needs to build this safely.
     If a section does not apply, write "none" rather than deleting it, so a
     reader knows it was considered. -->

## Goal

What outcome, in one or two sentences.

## Definition of done

The observable that proves it is done, and its negative.

- Observable: ...
- Negative: what must still FAIL when the work is done (the test or check that
  would go green if the work regressed).

## Scope

What changes, by file or area.

- ...

## Out of scope

What this story deliberately does not do, so the work does not creep.

- ...

## Consumers and surfaces touched

Every resource, endpoint, port, wire payload, command-line contract, or CRD
field this touches, and what must stay true for its consumer. Write "none" for
a pure docs change.

- ...

## Verification

The exact commands that must pass, and the expected result.

```sh
# ...
```

## Falsification

The production edit that must make a test or check fail, and the failure you
expect to see. If you cannot name one, the verification does not bite.

- Edit: ...
- Expected failure: ...

## Risks

What could go wrong, and the unknowns worth calling out before building.

- ...
