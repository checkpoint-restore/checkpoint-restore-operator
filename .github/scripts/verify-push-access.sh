#!/usr/bin/env bash
# Verify that the registry credentials in REGISTRY_USER/REGISTRY_PASS
# have push access to the given repositories, using the Docker Registry
# v2 token endpoint. The endpoint returns a JWT whose access claim
# lists the granted actions, so push permission can be checked without
# pushing anything.
#
# Usage: verify-push-access.sh AUTH_URL SERVICE REPOSITORY...
set -euo pipefail

auth_url="$1"
service="$2"
shift 2

for repo in "$@"; do
	token=$(curl -fsS -u "${REGISTRY_USER}:${REGISTRY_PASS}" \
		--get \
		--data-urlencode "service=${service}" \
		--data-urlencode "scope=repository:${repo}:pull,push" \
		"${auth_url}" | jq -r '.token')

	# Decode the base64url-encoded JWT payload.
	payload=$(printf '%s' "${token}" | cut -d. -f2 | tr '_-' '/+')
	while [ $((${#payload} % 4)) -ne 0 ]; do
		payload="${payload}="
	done
	actions=$(printf '%s' "${payload}" | base64 -d | jq -r \
		--arg repo "${repo}" \
		'.access[]? | select(.name == $repo) | .actions[]?')

	if ! printf '%s\n' "${actions}" | grep -qx push; then
		echo "::error::${service}: credentials lack push access to ${repo}"
		exit 1
	fi
	echo "${service}: push access to ${repo} verified"
done
