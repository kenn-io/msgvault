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
# Usage: scripts/test-package-shards.sh <package> [shards=4] [tags] [timeout=60m]
# Mirrors scripts/test-package-shards.ps1, which the Windows lanes use.
set -euo pipefail

package=${1:?package (e.g. ./internal/store)}
shard_count=${2:-4}
tags=${3:-}
timeout=${4:-60m}

package_dir=$(go list ${tags:+-tags "$tags"} -f '{{.Dir}}' "$package")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
test_binary="$work/tests"

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
	(
		cd "$package_dir"
		exec "$test_binary" "-test.run=$pattern" "-test.timeout=$timeout"
	) >"$work/shard-$i.out" 2>&1 &
	pids+=($!)
done

failed=0
for ((i = 0; i < active_shards; i++)); do
	count=$(( $(wc -l <"$work/shard-$i.names") ))
	if wait "${pids[$i]}"; then
		echo "ok shard $((i + 1)): $count tests"
	else
		failed=1
		echo "shard $((i + 1)) failed" >&2
		cat "$work/shard-$i.out"
	fi
done

exit $failed
