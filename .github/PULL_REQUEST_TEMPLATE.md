<!--
Title must be a conventional commit: feat:/fix:/docs:/chore:/deps:/ci: …
release-please derives the version bump and changelog from it.
-->

## What

<!-- one paragraph: the change and why -->

## How was it tested

<!-- unit tests / kind e2e (make e2e) / SKS / EKS hybrid — be specific -->

## Checklist

- [ ] `make generate manifests` — no drift (CI enforces)
- [ ] `make test lint helm-lint` pass locally
- [ ] docs updated (README / docs/ / examples) if behavior changed
- [ ] join-profile contract unchanged, or migration noted in the PR
