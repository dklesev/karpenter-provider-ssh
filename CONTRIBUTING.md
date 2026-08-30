# Contributing

Thanks for considering it.

## Ground rules

- **Conventional commits** — release-please derives versions and the
  changelog from them. `feat:` (minor), `fix:` and `deps:` (patch), `feat!:`
  or a `BREAKING CHANGE:` footer (minor while we are on 0.x, major after 1.0).
  Keep the subject under ~70 chars, imperative mood.
- **Behavior changes are never `chore:`, `test:` or `ci:`.** Those three types
  are hidden from the changelog, which means they do not refresh the release
  PR — and the release PR is where the e2e suite runs. A behavior change
  smuggled in under one of them can reach a tag having never been e2e'd. If it
  touches what the controller does, it is a `fix:`, `feat:` or `refactor:`.
  When unsure, use `fix:`. (`docs:` *does* produce a changelog entry and does
  refresh the release PR — it is not in this list.)
- **e2e runs at release time, not per commit** — the kind suite fans one
  scenario per runner (a matrix) and burns runner-minutes accordingly, so it
  gates the release-please PR instead of every push (see
  [docs/development.md](docs/development.md)). A maintainer can comment `/e2e`
  on any PR to run it there.
- **Green locally before pushing**:

  ```bash
  make verify                              # codegen + chart README + license headers: must produce no diff
  make test lint lint-shell lint-actions helm-lint
  ```

  That is the whole CI gate. `make verify` is the exact equivalent of CI's
  codegen job — it also runs `helm-docs` and the license check, so a
  `values.yaml` edit without it reds the build.

- **Docs move with behavior.** A PR that changes flags, CRD fields, profile
  contract or chart values updates `docs/`, `examples/` and (for values)
  `make helm-docs` in the same PR.
- **Idempotency is the contract.** Anything on the claim/join/leave path must
  survive being re-run at any point. Tests or a convincing argument required.
- **No karpenter forks.** Core is a library; if upstream behavior blocks you,
  the fix is an issue/PR upstream or a CloudProvider-surface workaround.

## Developing

See [docs/development.md](docs/development.md) for the build/run/e2e story and
repo layout.

## Pull requests

1. Fork/branch from `main`.
2. Small, reviewable commits — each one green.
3. Fill the PR template (what/why, how tested — name the environment: unit /
   kind e2e (`make e2e`) / SKS / EKS hybrid).
4. CI must pass; codegen drift and chart/CRD desync fail the build.
5. Use a conventional-commit PR **title**: it becomes the squash-merge commit
   message, and that is what release-please reads.

## Reporting bugs

Use the bug template. The three things that make an SSH-provider bug
debuggable: controller log lines (JSON), `SSHHost` status
(`kubectl get sshhost -o yaml`), and the NodeClaim events. Redact endpoints,
tokens and activation codes.

## Security issues

Not in public issues — see [SECURITY.md](SECURITY.md).

## License of contributions

By contributing you agree your contribution is licensed under
[Apache-2.0](LICENSE), the project license (inbound=outbound — this is a
deliberate choice instead of a DCO sign-off; no `Signed-off-by` required).
Go files carry the Apache-2.0 header; `make license` adds it, CI checks it.
