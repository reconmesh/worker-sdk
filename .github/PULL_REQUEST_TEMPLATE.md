<!-- One-line summary in the PR title. Conventional Commits style:
     feat(scope): / fix(scope): / docs(scope): / test(scope): / chore(scope): -->

## What

<!-- One paragraph: what changed and why. The diff shows the what,
     this prose explains the why. -->

## Test plan

<!-- Concrete commands you ran. Examples:
     - [ ] `make vet` clean
     - [ ] `make test` clean
     - [ ] `make check-manifests` clean (if you touched a manifest.yaml)
     - [ ] manually exercised <feature> via <UI / curl / docker compose up>
-->

## Stage tag

<!-- If this lands a Stage A-K item from the plan, mention it:
     "Closes Stage I12" / "Part of Stage H4" / "Forward-looking C7".
     Skip if it's pure maintenance (chore / docs / ci). -->

## Breaking changes

<!-- worker-sdk wire contract: any change to Manifest / CascadeArgs /
     Result / Tool interface needs a MAJOR bump.
     SQL migration: any non-additive change needs the down.sql to
     reverse cleanly.
     Default → !default config flip: call out in the body so operators
     read the diff before pulling latest. -->

None.
