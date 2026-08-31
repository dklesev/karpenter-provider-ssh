# Development

## Toolchain

Go 1.27, helm ≥ 3.13 (the same floor users install with — see
[Installation](installation.md#prerequisites)), docker (buildx for multi-arch),
and `shellcheck` (for the
shipped shell; ubuntu runners and homebrew both have it).

Everything else needs **no install**. `controller-gen`, `setup-envtest` and
`addlicense` are pinned in go.mod's `tool` block and run with `go tool`;
`helm-docs`, `actionlint` and `golangci-lint` are pinned in the Makefile and
run with `go run …@version`, so `make lint`, `make lint-actions` and
`make helm-docs` are byte-for-byte the versions CI uses — one source of truth,
dependabot bumps them,
resolution is reproducible.

## Everyday loop

```bash
make build             # generate deepcopy + compile → bin/controller
make test              # go test ./...
make test-integration  # envtest suite: real apiserver + informer cache (downloads kubebuilder assets on first run)
make lint              # golangci-lint (same config as CI)
make manifests         # regenerate CRDs → config/crd/ AND charts/…/crds/
make helm-lint         # helm lint + template smoke
make helm-docs         # regenerate chart README from values.yaml annotations
make lint-docs         # every relative markdown link + #anchor resolves
make docs-serve        # live-preview this site (zensical, same pin as CI)
```

`make docs` / `make docs-serve` install zensical into a throwaway venv under
`bin/`, so they touch nothing outside the repo. Preview before you push: the
docs job publishes on merge to main, so an unbuilt site is discovered by a
reader, not by CI.

Three test layers, each catching a class the previous one can't: unit tests
(fake client — logic), **envtest** (real apiserver + real informer cache —
caching/consistency semantics like read-your-own-writes; the zombie-guard CAS
regression lives here), and `make e2e` (kind + container hosts — full loop
over real SSH, including verified execution).

CI enforces on every PR: vet, `test -race`, envtest integration suite,
golangci-lint, shellcheck on the shipped shell (the shim + signer), helm
lint/template, chart-CRDs == config/crd, image build + CVE scan — and **no
codegen/docs/license drift**: `make verify` is byte-for-byte that CI job, so
a `values.yaml` edit without `make helm-docs`, or a dead doc link, reds the
build rather than reaching a reader. The whole gate locally:

```bash
make verify
make test lint lint-shell lint-actions helm-lint
```
The kind e2e suite is the **release gate**: it runs on demand via a
maintainer `/e2e` comment — once on the release-please PR right before it
merges (its head is exactly what gets tagged), or on any PR that warrants it.
It never runs automatically on pushes or rebases.

## Running out of cluster

```bash
KUBECONFIG=~/.kube/config POOL_NAMESPACE=kpssh-system \
  ./bin/controller --disable-leader-election
```

- `--disable-leader-election` is required out-of-cluster (no namespace to
  hold the lease) — in-cluster it stays on.
- Health probes bind :8081, metrics :8080 — clashes with other
  controller-runtime dev processes are the usual "port already in use".
- The controller needs SSH reach to the pool from your machine in this mode.

## e2e environments

| env | what it proves | notes |
|---|---|---|
| kind + container hosts (in-repo) | full loop hands-off: pending pod → join → Ready, consolidation → warm release, **reboot-safety** (a restarted warm host must not rejoin), zombie guard, and **verified execution** (signed join; a rogue signer rejected by the host shim; an unsigned profile rejected by the controller) | `make e2e`; in CI it gates the release PR and runs on `/e2e` — see [test/e2e/](https://github.com/dklesev/karpenter-provider-ssh/tree/main/test/e2e) |
| Exoscale SKS | real cloud control plane, unofficial external nodes | needs kubelet-csr-approver for metrics; pool VMs in same SG+zone |
| EKS Hybrid Nodes | the actual target: `nodeadm-ssm`, adopt providerID, warm/cold split, billing | real cluster required; see installation.md walkthrough |

The in-repo e2e is a Go harness (`go test`, build tag `e2e`): each scenario runs
in its **own ephemeral kind cluster** (karpenter core is cluster-global, so
scenarios can't share one) with `kindest/node`-based containers (plus sshd) as
SSH pool hosts — same kubelet version as the control plane, CNI/pause images
pre-baked, and a container restart is a faithful reboot simulation (how the
reboot scenario stays CI-runnable). Locally `make e2e` runs them in parallel;
in CI a **matrix puts one scenario per runner**. `make e2e-one E2E_RUN=<Test>`
runs a single scenario.

## Commit types and what they cost

release-please derives the version and the changelog from the commit type
(`release-please-config.json`), and the release PR is also the e2e gate — so
the type you pick decides both whether a change ships and whether it is tested
before it does.

| type | changelog | release |
|---|---|---|
| `feat:` | Features | **minor** |
| `fix:` | Bug Fixes | **patch** |
| `deps:` | Dependencies | **patch** — release-please's default strategy bumps `deps` like `fix` |
| `feat!:` / `BREAKING CHANGE:` footer | called out at the top | **minor** on 0.x (`bump-minor-pre-major: true`), major after 1.0 — see [Cutting 1.0.0](#cutting-100) |
| `refactor:` `perf:` `build:` `style:` | own section | visible, but **no bump on their own**: if the change alters behavior, make it a `fix:` or `feat:` |
| `docs:` `chore:` `test:` `ci:` | `docs` visible, rest hidden | **no release PR of their own** |

> **Rule: a behavior-affecting change is `feat:`, `fix:` or `refactor:` —
> never `chore:`, `test:` or `ci:`.**
>
> The hidden types produce no changelog entry, so they may not update an open
> release PR — a behavior change wearing a `chore:` hat can therefore ride
> into the merge (and into the tag) on a head the `/e2e` gate never ran on.
> The commit type is not cosmetics here; it is what decides whether the
> release is tested.

Dependabot follows the same rule: `gomod` and `docker` bumps land as `deps:`
— which cuts a patch release, so a CVE fix in the shipped binary reaches users
— while `github-actions` bumps stay `ci:` (they change how we build, never
what we ship).

## Release process

Releases are cut by **release-please** from conventional commits on `main`:

1. Land commits with proper types (table above). Breaking changes: `feat!:` or
   a `BREAKING CHANGE:` footer.
2. release-please maintains a release PR (changelog + version bumps —
   `Chart.yaml` version/appVersion are bumped via the
   `x-release-please-version` markers).
3. Every time that PR is created or rebased, `release.yaml` dispatches
   `ci.yaml` onto its head (Go build + test, golangci-lint, generated-code
   drift, helm chart) and resets the `e2e` commit status to **pending** —
   the kind suite itself is not auto-dispatched (every push to main rebases
   the PR, so that would run the full matrix on every commit). The release PR
   head is exactly what gets tagged, so that is where everything must be
   green. Make all five contexts required status checks on `main`.
   (`ci.yaml`'s Go/lint/codegen jobs skip the release PR's `pull_request`
   events on purpose — they would duplicate the dispatched run.)
4. When the PR is ready to merge, comment `/e2e` on it — the kind suite runs
   against its head and reports the `e2e` status. Then merge the release PR →
   tag `vX.Y.Z` + GitHub release. (A rebase between `/e2e` and merge resets
   `e2e` to pending — comment again.)
5. CI publishes on release:
   - multi-arch image `ghcr.io/dklesev/karpenter-provider-ssh:vX.Y.Z` (+ `latest`)
   - helm chart `oci://ghcr.io/dklesev/charts/karpenter-provider-ssh:X.Y.Z`
   - both cosign-signed + attested (see [SECURITY.md](https://github.com/dklesev/karpenter-provider-ssh/blob/main/SECURITY.md))

No manual version edits, no manual tags. Manual publishing (emergency only):
`make docker-buildx IMG=… TAG=…` and `helm package charts/… && helm push …`.

### Running e2e on a feature PR

Feature PRs do **not** run the kind suite (~15 min of runner time each); their
`e2e` status is satisfied by `e2e-exempt.yaml`. When a PR touches the claim,
join or leave paths, a maintainer runs the real thing by commenting:

```text
/e2e
```

on the PR. The run executes that PR's code and overwrites the `e2e` status on
its head SHA — which also means: **review a fork PR before commenting `/e2e`
on it.** `workflow_dispatch` covers manual runs on any branch.

### Cutting 1.0.0

`bump-minor-pre-major: true` means a breaking change on 0.x bumps the **minor**
(0.2.1 → 0.3.0), not the major. That is deliberate: pre-1.0, breaking changes
are expected, and none of them should silently promote the project to a
stability promise it has not made. The flip side is that no commit type will
ever produce 1.0.0 on its own — the version would climb 0.3, 0.4, 0.5 forever.

1.0.0 is therefore a **declaration**, not a side effect. Make it with a
`Release-As:` footer on a commit to `main`:

```
chore: release 1.0.0

Release-As: 1.0.0
```

release-please reads the footer and opens a release PR pinned to exactly that
version, regardless of what the commits since the last tag would have computed.
Everything else is unchanged: the PR still gets the full dispatched check set
on its head, and merging it still tags and publishes.

After 1.0.0 the setting is inert — it only applies below 1.0.0 — and semver
resumes its normal meaning: `feat!:` bumps the major. Nothing needs editing in
`release-please-config.json`, and `.release-please-manifest.json` should not be
hand-edited to get there; the footer is the supported path.

The one thing worth settling before you use it: what 1.0.0 says about the API.
The API is `v1beta1`; the release notes should state the policy plainly —
breaking API changes bump the minor version and ship with migration steps,
until a battle-tested `v1`.

## Repo layout

```
cmd/controller/         entrypoint: karpenter core operator + our providers
pkg/apis/v1alpha1/      SSHHost, SSHNodeClass, SSHJoinProfile
pkg/cloudprovider/      karpenter CloudProvider implementation
pkg/providers/          host (inventory+claim), instance (lifecycle), instancetype, bootstrap
pkg/controllers/        hostprobe, nodeclass readiness
pkg/operator/           wiring around karpenter's operator
pkg/metrics/            provider Prometheus metrics + pool-inventory collector
internal/profile/       script template rendering + env assembly
internal/sshexec/       SSH transport: raw (TOFU, sudo bash -s) + verified (SSHSIG, envelope)
shim/                   kpssh-shim — the host half of verified exec, SHIPPED (release asset)
config/crd/             generated provider CRDs (source of truth)
config/karpenter/       vendored karpenter.sh CRDs (make karpenter-crds)
charts/                 helm chart (crds/ mirrors config/crd — CI-enforced)
examples/               copy-paste manifests for every object
test/e2e/               kind-based full-loop scenarios (build tag e2e)
hack/                   maintainer tooling only — never shipped to a user
docs/                   you are here
```

`shim/` is deliberately not under `hack/`: everything in `hack/` is ours, but
the shim runs as root on every host in a user's pool. It ships as a versioned,
attested release asset — see [shim/README.md](https://github.com/dklesev/karpenter-provider-ssh/blob/main/shim/README.md).

## Conventions

- Karpenter core is a dependency, not a fork — behavior differences belong in
  the CloudProvider surface, never in patched core.
- Every claim-path mutation must stay CAS-safe and resumable; assume the
  process dies between any two lines.
- Profile scripts in examples are contract documentation — keep them
  idempotent and boring.
- User-facing changes update `docs/` in the same PR (CI has no docs gate;
  reviewers do).
