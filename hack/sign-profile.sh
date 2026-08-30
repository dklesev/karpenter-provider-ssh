#!/usr/bin/env bash
# sign-profile.sh — sign a directory of phase scripts for verified execution and
# print the SSHJoinProfile `signatures:` block, ready to paste under `spec:`.
#
# Copyright The karpenter-provider-ssh Authors.
# Licensed under the Apache License, Version 2.0.
#
# Usage:   hack/sign-profile.sh <signing-key> <script-dir>
#   <signing-key>  an SSH private key (ssh-keygen -t ed25519 -f kpssh-signer)
#   <script-dir>   holds install.sh / join.sh / leave.sh / uninstall.sh (any subset)
#
# The signing key stays offline — run this in CI or on a trusted workstation,
# never on the controller or a pool host. Namespace is fixed to `kpssh`
# (override with KPSSH_NAMESPACE).
set -euo pipefail

KEY="${1:?usage: sign-profile.sh <signing-key> <script-dir>}"
DIR="${2:?usage: sign-profile.sh <signing-key> <script-dir>}"
NS="${KPSSH_NAMESPACE:-kpssh}"

echo "  signatures:"
for phase in install join leave uninstall; do
	f="$DIR/$phase.sh"
	[ -f "$f" ] || continue
	printf '    %s: |\n' "$phase"
	# `-Y sign` reading stdin writes the armored signature to stdout.
	ssh-keygen -Y sign -q -f "$KEY" -n "$NS" < "$f" | sed 's/^/      /'
done
