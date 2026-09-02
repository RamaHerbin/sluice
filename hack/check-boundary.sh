#!/usr/bin/env bash
# check-boundary.sh: enforce the two import boundaries from CLAUDE.md.
#
#   1. Only internal/controller may depend on k8s.io/*. Every other package
#      under internal/ is checked with "go list -deps", which follows the whole
#      transitive graph, so an indirect leak is caught too. cmd/sluice is not
#      checked for this rule: it wires the controller in, so it necessarily
#      depends on k8s.io.
#   2. Only internal/observability may import github.com/prometheus/*.
#      Transitively, cmd/sluice depends on it through observability, so the
#      transitive check exempts cmd/sluice and a second check verifies that
#      cmd/sluice does not import Prometheus directly.
#
# Exit status is non-zero when any rule is violated; the message names the
# package and the offending import paths.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
	echo "check-boundary: go not found in PATH" >&2
	exit 1
fi

module=$(go list -m)
packages=$(go list ./...)
fail=0

# forbid <package (relative)> <deps, one per line> <regex prefix> <rule text>
forbid() {
	local rel=$1 deps=$2 prefix=$3 rule=$4
	local hits count
	hits=$(printf '%s\n' "$deps" | grep -E "^${prefix}" || true)
	if [ -z "$hits" ]; then
		return 0
	fi
	count=$(printf '%s\n' "$hits" | wc -l | tr -d ' ')
	echo "check-boundary: FAIL: $rel depends on $count ${prefix//\\/} package(s); $rule" >&2
	printf '%s\n' "$hits" | head -n 10 | sed 's/^/    /' >&2
	if [ "$count" -gt 10 ]; then
		echo "    ... and $((count - 10)) more" >&2
	fi
	fail=1
	pkg_fail=1
}

echo "check-boundary: module $module"
for pkg in $packages; do
	rel=${pkg#"$module/"}
	deps=$(go list -deps "$pkg")
	pkg_fail=0

	case "$rel" in
	internal/controller | internal/controller/*) ;; # the one allowed k8s.io consumer
	internal/*) forbid "$rel" "$deps" 'k8s\.io/' "only internal/controller may import k8s.io" ;;
	esac

	case "$rel" in
	internal/observability | cmd/sluice) ;; # allowed prometheus consumers
	*) forbid "$rel" "$deps" 'github\.com/prometheus/' "only internal/observability may import github.com/prometheus" ;;
	esac

	if [ "$pkg_fail" -eq 0 ]; then
		echo "check-boundary: ok  $rel"
	fi
done

# cmd/sluice may only reach Prometheus through internal/observability.
direct=$(go list -f '{{join .Imports "\n"}}' ./cmd/sluice | grep -E '^github\.com/prometheus/' || true)
if [ -n "$direct" ]; then
	echo "check-boundary: FAIL: cmd/sluice imports Prometheus directly; go through internal/observability" >&2
	printf '%s\n' "$direct" | sed 's/^/    /' >&2
	fail=1
else
	echo "check-boundary: ok  cmd/sluice has no direct Prometheus import"
fi

if [ "$fail" -ne 0 ]; then
	echo "check-boundary: FAILED" >&2
	exit 1
fi
echo "check-boundary: all boundaries respected"
