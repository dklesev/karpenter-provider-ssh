# Support

## Where to ask

| you have | go to |
|---|---|
| a question, a "how do I…", an unsure "is this a bug?" | [GitHub Discussions](https://github.com/dklesev/karpenter-provider-ssh/discussions) |
| a reproducible bug | [Bug report](https://github.com/dklesev/karpenter-provider-ssh/issues/new?template=bug_report.yaml) |
| a missing capability | [Feature request](https://github.com/dklesev/karpenter-provider-ssh/issues/new?template=feature_request.yaml) |
| a vulnerability | **not** a public issue — [SECURITY.md](SECURITY.md) |

Read [docs/troubleshooting.md](docs/troubleshooting.md) first; it is
symptom-indexed and covers the failure modes that actually happen (hosts stuck
`Unhealthy`, joins that never register, `adopt` mode not adopting, billing that
did not stop).

## What to include

An SSH-provider problem is almost always visible in these four things. Without
them a report is guesswork:

```bash
kubectl -n kpssh-system logs deploy/karpenter-provider-ssh --tail=200
kubectl -n kpssh-system get sshhost <host> -o yaml     # status: state, claimRef, lastProbeError
kubectl get nodeclaim <name> -o yaml                    # and: kubectl describe nodeclaim <name>
kubectl get snc -o wide                                 # SSHNodeClass Ready + reason
```

Plus: which join profile (`tls-bootstrap`, `nodeadm-ssm`, your own), which
control plane (kind, k0s, SKS, EKS Hybrid, …), and the chart version.

**Redact** endpoints, bootstrap tokens, activation ids/codes and CA bundles
before pasting.

## Expectations

This is a young project maintained in the open by one person with a day job.
Issues and discussions are read; nobody is on call. There is no SLA on
questions or bugs — the one commitment that does exist is on security reports
(see [SECURITY.md](SECURITY.md)).

Pull requests are the fastest path from "this is broken" to "this is fixed" —
see [CONTRIBUTING.md](CONTRIBUTING.md).

## Commercial support

None. If you need it, say so in a Discussion — demand is the only signal that
would change that.
