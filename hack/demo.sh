#!/usr/bin/env bash
# demo.sh: exercise a deployed sluice through the kind port mapping.
#
# Expects:
#   - a kind cluster from hack/kind-up.sh (host 127.0.0.1:8080 -> NodePort 30080)
#   - sluice deployed by "make deploy": namespace sluice-system, deployment sluice,
#     admin port 9090 inside the pod
#   - the demo manifests applied: namespace demo, deployments echo-a and echo-b,
#     ingress demo for host demo.localtest.me routing /a to echo-a and /b to echo-b
#
# The script asserts the status codes: 200 for /a and /b, 404 for an unknown
# host, 200 for /readyz and /metrics fetched through kubectl port-forward.
set -euo pipefail

PROXY_ADDR=${PROXY_ADDR:-127.0.0.1:8080}
DEMO_HOST=${DEMO_HOST:-demo.localtest.me}
ADMIN_PORT=${ADMIN_PORT:-9090}
SLUICE_NS=${SLUICE_NS:-sluice-system}
DEMO_NS=${DEMO_NS:-demo}

say() { printf '\n==> %s\n' "$*"; }
die() {
	echo "demo: $*" >&2
	exit 1
}

for tool in kubectl curl; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool not found in PATH"
done

pf_pid=""
body=""
cleanup() {
	if [ -n "$pf_pid" ]; then
		kill "$pf_pid" 2>/dev/null || true
		wait "$pf_pid" 2>/dev/null || true
	fi
	[ -n "$body" ] && rm -f "$body"
}
trap cleanup EXIT
body=$(mktemp)

# request <want status> <description> <curl args...>
# The response body lands in $body so the caller can print it.
request() {
	local want=$1 desc=$2
	shift 2
	local code
	code=$(curl -sS --max-time 10 -o "$body" -w '%{http_code}' "$@") || die "$desc: curl failed"
	if [ "$code" != "$want" ]; then
		echo "--- response body ---"
		cat "$body"
		echo
		die "$desc: got HTTP $code, want $want"
	fi
	echo "ok: $desc -> HTTP $code"
}

say "Waiting for deployment sluice in namespace $SLUICE_NS"
kubectl -n "$SLUICE_NS" rollout status deployment/sluice --timeout=120s

say "Waiting for the demo backends in namespace $DEMO_NS"
if ! kubectl get namespace "$DEMO_NS" >/dev/null 2>&1; then
	die "namespace $DEMO_NS not found. Apply the demo manifests first (namespace demo, deployments echo-a and echo-b, ingress demo)."
fi
kubectl -n "$DEMO_NS" rollout status deployment/echo-a --timeout=120s
kubectl -n "$DEMO_NS" rollout status deployment/echo-b --timeout=120s
kubectl -n "$DEMO_NS" get ingress demo

say "GET http://$PROXY_ADDR/a with Host: $DEMO_HOST (expect 200 from echo-a)"
request 200 "GET /a" -H "Host: $DEMO_HOST" "http://$PROXY_ADDR/a"
cat "$body"
echo

say "GET http://$PROXY_ADDR/b with Host: $DEMO_HOST (expect 200 from echo-b)"
request 200 "GET /b" -H "Host: $DEMO_HOST" "http://$PROXY_ADDR/b"
cat "$body"
echo

say "GET http://$PROXY_ADDR/ with an unknown host (expect 404)"
request 404 "GET / unknown host" -H "Host: unknown.example.invalid" "http://$PROXY_ADDR/"

say "Fetching metrics: kubectl port-forward deployment/sluice 127.0.0.1:$ADMIN_PORT -> 9090"
kubectl -n "$SLUICE_NS" port-forward deployment/sluice "$ADMIN_PORT:9090" >/dev/null 2>&1 &
pf_pid=$!
for ((i = 0; i < 40; i++)); do
	if curl -sS --max-time 2 -o /dev/null "http://127.0.0.1:$ADMIN_PORT/healthz" 2>/dev/null; then
		break
	fi
	sleep 0.5
done
request 200 "GET /readyz" "http://127.0.0.1:$ADMIN_PORT/readyz"
request 200 "GET /metrics" "http://127.0.0.1:$ADMIN_PORT/metrics"
echo "--- sluice_* metrics ---"
grep -E '^sluice_' "$body" || echo "(no sluice_ metric exposed yet)"

say "Demo complete"
