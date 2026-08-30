# kpssh-shim

The host side of [verified execution](../docs/verified-exec.md). It is pinned as
the pool user's sshd `ForceCommand`, so it is the only thing an SSH session to a
verified host can run: it reads an envelope on stdin, re-verifies the SSHSIG
signature against `/etc/kpssh/allowed_signers`, and only then executes the
script as root.

It re-verifies on purpose. The controller already checked the signature before
connecting, and the shim does not trust that — a controller with a stolen SSH
key is exactly the case this gate exists for. Two independent checks, one of
which is not reachable from the cluster.

POSIX `sh`, no dependencies beyond `ssh-keygen`. Runs on every host in the pool,
as root.

## Don't install this copy

Install the release asset, and verify it first — see
[Provision the host for verified mode](../docs/verified-exec.md#2-provision-the-host-for-verified-mode).
A file this privileged should reach your hosts with a version and a provenance
attestation attached, not as a `curl` of whatever `main` happens to say today.
This copy is the source; the release job publishes it as `kpssh-shim` plus
`kpssh-shim.sha256` and attests it.

## Changing it

`make lint-shell` gates it at the same severity as CI. It is also covered by
real tests, both of which run the actual file rather than a copy of its logic:

- `internal/sshexec/shim_test.go` — Go tests that pipe envelopes into it directly.
- `test/e2e` — `TestE2EVerifiedJoinAndLeave` and
  `TestE2EVerifiedRogueSignerRejectedByShim` install it into a live host image
  and drive it through sshd.

The parameter-name rule (`KPSSH_[A-Za-z0-9_]+`, enforced here *and* in
`internal/sshexec/envelope.go`) is load-bearing, not hygiene: the shim exports
each parameter before running the script under `bash`, and a name that is not a
plain identifier turns that export into command substitution. See
[what this does not close](../docs/verified-exec.md#what-this-does-not-close) for what this gate does
and does not cover.
