# Security Policy

## Supported versions

Pre-1.0: only the latest release receives fixes.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private vulnerability reporting:

<https://github.com/dklesev/karpenter-provider-ssh/security/advisories/new>

Include: affected version/commit, environment (control plane, profile), a
reproduction or proof of concept, and impact assessment if you have one.

You can expect an acknowledgement within a few days. Fixes ship as a patch
release with credit (unless you prefer otherwise) once a coordinated
disclosure makes sense.

## Release integrity

Every release is built by GitHub Actions from the tagged commit and is
verifiable end to end:

- **Signatures** — image and chart are both signed with cosign (keyless,
  GitHub OIDC identity):

  ```bash
  # container image
  cosign verify ghcr.io/dklesev/karpenter-provider-ssh:vX.Y.Z \
    --certificate-identity-regexp 'https://github.com/dklesev/karpenter-provider-ssh/\.github/workflows/release\.yaml@.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

  # helm chart (OCI)
  # note: the chart tag has NO leading v — helm push tags with the bare
  # Chart.yaml version, unlike the image, which is tagged vX.Y.Z
  cosign verify ghcr.io/dklesev/charts/karpenter-provider-ssh:X.Y.Z \
    --certificate-identity-regexp 'https://github.com/dklesev/karpenter-provider-ssh/\.github/workflows/release\.yaml@.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

- **SLSA provenance + SBOM attestations** (GitHub attestation API, also pushed
  to the registry). Provenance verifies by default; the SBOM attestation is
  selected by its predicate type:

  ```bash
  # build provenance
  gh attestation verify oci://ghcr.io/dklesev/karpenter-provider-ssh:vX.Y.Z \
    --repo dklesev/karpenter-provider-ssh

  # SBOM attestation
  gh attestation verify oci://ghcr.io/dklesev/karpenter-provider-ssh:vX.Y.Z \
    --repo dklesev/karpenter-provider-ssh \
    --predicate-type https://spdx.dev/Document
  ```

- **SBOM** — SPDX JSON attached to every GitHub release; BuildKit additionally
  embeds SBOM + provenance in the image manifest.
- **Immutability** — releases are never retagged or re-uploaded; GitHub's
  immutable-releases protection and a tag ruleset (no force-push, no deletion
  of `v*`) are enabled on the repository. Pin images by digest for the
  strongest guarantee (`ghcr.io/…@sha256:…`; the digest is in the release
  notes' attestation).
- **Continuous scanning** — govulncheck (call-graph Go CVEs), grype (image
  SBOM scan, fails on fixable high/critical), CodeQL (Go + workflows), OpenSSF
  Scorecard, dependency review on PRs, weekly scheduled re-scans.

## Scope notes

Particularly interesting areas: the SSH transport (host-key pinning, env
injection, quoting in `internal/sshexec`), bootstrap-token minting/scope,
secret material flow (`joinSecretRef` → script stream), and RBAC boundaries of
the helm chart.
