#!/usr/bin/env bash
# kind-up.sh: create the local kind cluster used by the demo, idempotently,
# then wait until its node reports Ready.
#
# Cluster name: $KIND_CLUSTER (default "sluice"). Config: deploy/kind/cluster.yaml,
# which maps host 127.0.0.1:8080 to NodePort 30080 inside the cluster.
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=${KIND_CLUSTER:-sluice}
CONFIG=${KIND_CONFIG:-deploy/kind/cluster.yaml}
CONTEXT="kind-$CLUSTER"

die() {
	echo "kind-up: $*" >&2
	exit 1
}

for tool in kind kubectl docker; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool not found in PATH (macOS: brew install kind kubernetes-cli)"
done
[ -f "$CONFIG" ] || die "config $CONFIG not found"

if ! docker info >/dev/null 2>&1; then
	die "the Docker daemon is not reachable. Start it first, for example: colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs"
fi

existing=$(kind get clusters 2>/dev/null || true)
if printf '%s\n' "$existing" | grep -qx "$CLUSTER"; then
	echo "kind-up: cluster '$CLUSTER' already exists, reusing it"
else
	echo "kind-up: creating cluster '$CLUSTER' from $CONFIG"
	kind create cluster --name "$CLUSTER" --config "$CONFIG"
fi

echo "kind-up: waiting for the node(s) of '$CLUSTER' to be Ready"
kubectl --context "$CONTEXT" wait --for=condition=Ready node --all --timeout=180s
kubectl --context "$CONTEXT" get nodes -o wide

echo "kind-up: done. kubectl context: $CONTEXT"
echo "kind-up: next: make docker-build kind-load deploy, then apply the demo manifests and run make demo"
