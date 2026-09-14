#!/usr/bin/env bash
set -euo pipefail

image="${1:?Usage: scripts/smoke-container.sh IMAGE}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker run --rm --network none "$image" version
docker run --rm --network none "$image" --help

docker run --rm --network none -i --entrypoint sh "$image" -s \
  < "$script_dir/smoke-image.sh"
