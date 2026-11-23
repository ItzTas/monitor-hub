#!/usr/bin/env bash
# Tags and publishes one release stream from the outputs of github-tag-action run
# with dry_run, using the version next-version.sh picks.
# Env: TAG_PREFIX RELEASE_TYPE PREVIOUS_VERSION NEW_VERSION CHANGELOG
#      GITHUB_SHA GITHUB_REPOSITORY GITHUB_OUTPUT GH_TOKEN
set -euo pipefail

# The action wrote its own version into the changelog heading; swap in the final one.
retarget_notes() {
	local notes=$1 old_tag=$2 old_version=$3 tag=$4 version=$5
	local out='' line seen=0
	if [[ $old_tag == "$tag" ]]; then
		printf '%s' "$notes"
		return
	fi
	while IFS= read -r line || [[ -n $line ]]; do
		if ((!seen)) && [[ $line == '#'* ]]; then
			line=${line//"$old_tag"/"$tag"}
			line=${line//"$old_version"/"$version"}
			seen=1
		fi
		out+=$line$'\n'
	done <<<"$notes"
	printf '%s' "${out%$'\n'}"
}

main() {
	local dir version tag notes
	dir=$(dirname "${BASH_SOURCE[0]}")
	version=$("$dir/next-version.sh" "$RELEASE_TYPE" "$PREVIOUS_VERSION" "$NEW_VERSION")
	if [[ -z $version ]]; then
		return
	fi
	tag=$TAG_PREFIX$version
	notes=$(retarget_notes "$CHANGELOG" "$TAG_PREFIX$NEW_VERSION" "$NEW_VERSION" "$tag" "$version")

	git -c user.name='github-actions[bot]' \
		-c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
		tag -a "$tag" -m "$tag" "$GITHUB_SHA"
	git push origin "refs/tags/$tag"
	gh release create "$tag" --repo "$GITHUB_REPOSITORY" --title "$tag" --notes "$notes"
	echo "version=$version" >>"$GITHUB_OUTPUT"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
	main
fi
