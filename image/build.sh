#!/bin/bash
# Builds the runner image and pulls the pinned BuildKit image, then prints
# the `runner.image` and `buildkit.image` values for the scaler config.
#
#   image/build.sh            build the runner image
#   image/build.sh --record   also build the diagnostic hook variant, which
#                             writes how the runner started the hook, the
#                             environment variable names (no values), the
#                             SigIgn/SigBlk lines of its /proc status,
#                             whether GITHUB_EVENT_NAME and GITHUB_EVENT_PATH
#                             are set, the event file size and a same,
#                             different, missing or null word per repository
#                             ID pair (never the IDs) to ~/.scaler/hook-record
#                             before its first request, then the status and
#                             time of its first event report
set -euo pipefail

# moby/buildkit v0.35.1 rootless, image index digest from
# `docker buildx imagetools inspect moby/buildkit:v0.35.1-rootless`.
# https://github.com/moby/buildkit/releases/tag/v0.35.1
# https://hub.docker.com/r/moby/buildkit
BUILDKIT_IMAGE=moby/buildkit:v0.35.1-rootless@sha256:7d02292556e66feea6d2d89b7ec143503b57e4f59a4efc6b7ece8547e4d3e5be
RUNNER_TAG=actions-runner-scaler-runner:local
RECORD_TAG=actions-runner-scaler-runner:record
PLATFORM=linux/arm64

record=0
case "${1:-}" in
'') ;;
--record) record=1 ;;
*)
	echo "usage: $0 [--record]" >&2
	exit 2
	;;
esac

dir=$(cd "$(dirname "$0")" && pwd)

check_arch() {
	local arch
	arch=$(docker image inspect --format '{{.Architecture}}' "$1")
	if [ "$arch" != arm64 ]; then
		echo "$1 is $arch, want arm64" >&2
		exit 1
	fi
}

docker pull --platform "$PLATFORM" "$BUILDKIT_IMAGE"
check_arch "$BUILDKIT_IMAGE"

# buildx with --load: a builder chosen by `docker buildx use` or BUILDX_BUILDER
# whose driver is not `docker` (docker-container, remote, kubernetes) keeps
# the result in its build cache unless --load imports it into the image store.
# https://docs.docker.com/build/builders/drivers/
# https://docs.docker.com/build/builders/
docker buildx build --load --platform "$PLATFORM" --tag "$RUNNER_TAG" "$dir"
check_arch "$RUNNER_TAG"
runner_id=$(docker image inspect --format '{{.Id}}' "$RUNNER_TAG")

if [ "$record" = 1 ]; then
	docker buildx build --load --platform "$PLATFORM" --build-arg HOOK_RECORD=1 --tag "$RECORD_TAG" "$dir"
	check_arch "$RECORD_TAG"
	record_id=$(docker image inspect --format '{{.Id}}' "$RECORD_TAG")
fi

echo
echo "# scaler config values"
printf 'runner.image = "%s"\n' "$runner_id"
printf 'buildkit.image = "%s"\n' "$BUILDKIT_IMAGE"
if [ "$record" = 1 ]; then
	printf '# diagnostic hook variant (%s): runner.image = "%s"\n' "$RECORD_TAG" "$record_id"
fi
