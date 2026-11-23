#!/usr/bin/env bash
# Prints the version a release takes. While a stream is in 0.x, a breaking change
# bumps the minor and not the major; v1.0.0 comes only when the owner says so.
# Prints nothing when there is no release (NEW_VERSION is empty).
# Usage: next-version.sh RELEASE_TYPE PREVIOUS_VERSION NEW_VERSION
set -euo pipefail

release_type=${1-}
previous=${2-}
new=${3-}

if [[ -z $new ]]; then
	exit 0
fi

semver='^[0-9]+\.[0-9]+\.[0-9]+$'
if ! [[ $previous =~ $semver && $new =~ $semver ]]; then
	echo "not a version: previous '$previous', new '$new'" >&2
	exit 1
fi

IFS=. read -r major minor _ <<<"$previous"
if [[ $release_type == major && $major == 0 ]]; then
	echo "0.$((10#$minor + 1)).0"
	exit 0
fi
echo "$new"
