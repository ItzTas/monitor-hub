#!/usr/bin/env bash
# Local test of the release scripts: .github/scripts/release.test.sh
set -uo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=tag-release.sh
source "$dir/tag-release.sh"
failed=0

check() {
	local name=$1 want=$2 got=$3
	if [[ $got == "$want" ]]; then
		echo "ok   $name"
		return
	fi
	echo "FAIL $name: want '$want', got '$got'"
	failed=1
}

version() {
	"$dir/next-version.sh" "$@" 2>&1
}

check "0.1.0 + major" 0.2.0 "$(version major 0.1.0 1.0.0)"
check "0.6.0 + minor" 0.7.0 "$(version minor 0.6.0 0.7.0)"
check "0.4.0 + patch" 0.4.1 "$(version patch 0.4.0 0.4.1)"
check "1.2.3 + major" 2.0.0 "$(version major 1.2.3 2.0.0)"
check "0.9.0 + major" 0.10.0 "$(version major 0.9.0 1.0.0)"
check "0.0.0 + major (first release)" 0.1.0 "$(version major 0.0.0 1.0.0)"
check "no release" "" "$(version '' 0.4.0 '')"
check "garbage version" "not a version: previous 'x', new '1.0.0'" "$(version major x 1.0.0)"

notes=$'## [1.0.0](https://github.com/o/r/compare/sdk-go/v0.1.0...sdk-go/v1.0.0) (2026-11-23)\n\n### Fixes\n\n* keep 1.0.0 in text\n'
want=$'## [0.2.0](https://github.com/o/r/compare/sdk-go/v0.1.0...sdk-go/v0.2.0) (2026-11-23)\n\n### Fixes\n\n* keep 1.0.0 in text'
check "changelog heading" "$want" "$(retarget_notes "$notes" sdk-go/v1.0.0 1.0.0 sdk-go/v0.2.0 0.2.0)"
check "changelog unchanged" "${notes%$'\n'}" "$(retarget_notes "$notes" v0.7.0 0.7.0 v0.7.0 0.7.0)"

exit "$failed"
