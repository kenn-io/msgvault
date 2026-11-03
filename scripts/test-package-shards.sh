#!/usr/bin/env bash
# Run one Go package's tests as several concurrent processes.
#
# A package's tests run one at a time inside a single test binary, so a package
# with thousands of tests sets the wall clock for the whole job no matter how
# many cores the runner has. This compiles the package once, lists its tests,
# deals them round-robin into shards, and runs each shard as its own process
# with -test.run pinned to exactly its names. The tests, their order within a
# shard, and the binary are unchanged; only the process boundary is new.
#
# Every shard's test2json events are collected in gotestsum/<suite>.json under
# the current directory, the file CI uploads as an artifact. The suite defaults
# to the package's last path element.
#
# Usage: scripts/test-package-shards.sh <package> [shards=4] [tags] [timeout=60m] [suite]
# Mirrors scripts/test-package-shards.ps1, which the Windows lanes use.
set -euo pipefail

package=${1:?package (e.g. ./internal/store)}
shard_count=${2:-4}
tags=${3:-}
timeout=${4:-60m}

package_info=$(go list ${tags:+-tags "$tags"} -f '{{.Dir}}{{"\t"}}{{.ImportPath}}' "$package")
IFS=$'\t' read -r package_dir import_path <<<"$package_info"
suite=${5:-${import_path##*/}}
json="$PWD/gotestsum/$suite.json"
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
test_binary="$work/tests"

# gotestsum is a tool dependency of this module, which the package under test
# need not belong to. test2json turns each shard's -test.v=test2json output
# into the events go test -json prints; gotestsum records them and prints them
# as verbose test output.
(cd "$script_dir" && go build -o "$work/gotestsum" gotest.tools/gotestsum && go build -o "$work/test2json" cmd/test2json)

go test -c -o "$test_binary" ${tags:+-tags "$tags"} "$package"
if [ ! -x "$test_binary" ]; then
	# go test -c writes nothing for a package without test files.
	echo "No tests found in $package"
	exit 0
fi

# Listing runs the package's init and TestMain, so a failure there is a real
# failure, not an empty package. Kept out of a process substitution and read
# with a loop so both the exit status and macOS's Bash 3.2 are honored.
names_file="$work/names"
if ! "$test_binary" '-test.list=^(Test|Example|Fuzz)' >"$names_file"; then
	echo "Listing tests in $package failed" >&2
	exit 1
fi
test_names=()
while IFS= read -r name; do
	test_names+=("$name")
done <"$names_file"
if [ "${#test_names[@]}" -eq 0 ]; then
	echo "No tests found in $package"
	exit 0
fi

active_shards=$shard_count
if [ "${#test_names[@]}" -lt "$active_shards" ]; then
	active_shards=${#test_names[@]}
fi

echo "Running ${#test_names[@]} tests from $package in $active_shards shards"

for ((i = 0; i < active_shards; i++)); do
	: >"$work/shard-$i.names"
done
for ((i = 0; i < ${#test_names[@]}; i++)); do
	printf '%s\n' "${test_names[$i]}" >>"$work/shard-$((i % active_shards)).names"
done

pids=()
for ((i = 0; i < active_shards; i++)); do
	# Test names are Go identifiers: no regex metacharacters to escape.
	pattern="^($(paste -sd '|' "$work/shard-$i.names"))$"
	# The time keyword reports the shard's wall clock for its package line
	# without needing a sub-second clock that macOS's Bash 3.2 lacks.
	(
		cd "$package_dir"
		TIMEFORMAT=%3R
		{ time "$work/gotestsum" --format standard-verbose --format-icons text --jsonfile "$work/shard-$i.json" \
			--raw-command -- "$work/test2json" -t -p "$import_path" \
			"$test_binary" -test.v=test2json "-test.run=$pattern" "-test.timeout=$timeout" >"$work/shard-$i.out" 2>&1; } 2>"$work/shard-$i.time"
	) &
	pids+=($!)
done

# Each shard's results end with the package line go test prints, so a reader
# of the log can tell which package the named results above it belong to.
elapsed() {
	local seconds
	seconds=$(tail -n 1 "$work/shard-$1.time" 2>/dev/null) || true
	printf '%ss' "${seconds:-0.000}"
}

mkdir -p "${json%/*}"
: >"$json"
failed=0
for ((i = 0; i < active_shards; i++)); do
	count=$(( $(wc -l <"$work/shard-$i.names") ))
	status=0
	wait "${pids[$i]}" || status=$?
	if [ -f "$work/shard-$i.json" ]; then
		cat "$work/shard-$i.json" >>"$json"
	fi
	if [ "$status" -eq 0 ]; then
		awk '/^[[:space:]]*--- (PASS|SKIP): /' "$work/shard-$i.out"
		printf 'ok  \t%s\t%s\n' "$import_path" "$(elapsed "$i")"
		echo "ok shard $((i + 1)): $count tests"
	else
		failed=1
		echo "shard $((i + 1)) failed" >&2
		cat "$work/shard-$i.out"
		printf 'FAIL\t%s\t%s\n' "$import_path" "$(elapsed "$i")"
	fi
done

exit $failed
