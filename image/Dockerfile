# Runner image for actions-runner-scaler (linux/arm64 only). Build it with
# build.sh, which prints the image ID to put in the scaler config.
#
# Pinned versions and their sources:
# - runner v2.338.2, image index digest
#   `docker buildx imagetools inspect ghcr.io/actions/actions-runner:2.338.2`
#   https://github.com/actions/runner/pkgs/container/actions-runner
#   The hook design relies on this runner version's source; re-check the hook
#   invocation and environment before changing it.
# - aws CLI 2.42.2: sha256 of the downloaded zip, whose PGP signature was
#   verified with the key in
#   https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
#   https://awscli.amazonaws.com/awscli-exe-linux-aarch64-2.42.2.zip
# - kubectl v1.37.2:
#   https://dl.k8s.io/release/v1.37.2/bin/linux/arm64/kubectl.sha256
# - docker CLI 29.10.0 (static archive; only the client is installed):
#   https://download.docker.com/linux/static/stable/aarch64/docker-29.10.0.tgz.sha256
# - buildx v0.39.1:
#   https://github.com/docker/buildx/releases/download/v0.39.1/checksums.txt
FROM ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300

ARG AWSCLI_VERSION=2.42.2
ARG AWSCLI_SHA256=efda2e5d9d01726f47c491a7b006f55611e682a0bdb08f4d65594e27bdc6ab30
ARG KUBECTL_VERSION=v1.37.2
ARG KUBECTL_SHA256=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
ARG DOCKER_VERSION=29.10.0
ARG DOCKER_SHA256=6e5159e8b5615e0c0c2954a2a1a2a12bc65f5d9b603595dba24a10cb367408cd
ARG BUILDX_VERSION=v0.39.1
ARG BUILDX_SHA256=05c88e0f9152825e609a1cdb391faf14d5f94d23d071eaecb87bae69d98da8b8

USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; \
    test "$(uname -m)" = aarch64; \
    tmp="$(mktemp -d)"; \
    cd "$tmp"; \
    curl -fsSLo awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-aarch64-${AWSCLI_VERSION}.zip"; \
    echo "${AWSCLI_SHA256}  awscliv2.zip" | sha256sum -c -; \
    unzip -q awscliv2.zip; \
    ./aws/install; \
    curl -fsSLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/arm64/kubectl"; \
    echo "${KUBECTL_SHA256}  kubectl" | sha256sum -c -; \
    install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl; \
    curl -fsSLo docker.tgz "https://download.docker.com/linux/static/stable/aarch64/docker-${DOCKER_VERSION}.tgz"; \
    echo "${DOCKER_SHA256}  docker.tgz" | sha256sum -c -; \
    tar -xzf docker.tgz docker/docker; \
    install -o root -g root -m 0755 docker/docker /usr/bin/docker; \
    curl -fsSLo docker-buildx "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-arm64"; \
    echo "${BUILDX_SHA256}  docker-buildx" | sha256sum -c -; \
    install -o root -g root -m 0755 -D docker-buildx /usr/local/lib/docker/cli-plugins/docker-buildx; \
    cd /; \
    rm -rf "$tmp"; \
    # The base image ships the whole static Docker bundle; jobs get no daemon.
    rm -f /usr/bin/dockerd /usr/bin/containerd /usr/bin/containerd-shim-runc-v2 \
        /usr/bin/ctr /usr/bin/runc /usr/bin/docker-proxy /usr/bin/docker-init; \
    if ! command -v git >/dev/null; then \
        apt-get update; \
        apt-get install -y --no-install-recommends git; \
        rm -rf /var/lib/apt/lists/*; \
    fi; \
    # Jobs run without privileges: no sudo grant and no docker group.
    gpasswd -d runner sudo; \
    gpasswd -d runner docker

COPY entrypoint.sh /usr/local/bin/scaler-entrypoint
COPY job-started.sh /usr/local/bin/scaler-job-started.sh

# 1 builds the diagnostic hook variant (build.sh --record). Declared late so
# both variants share the tool layer above. That variant compares the event
# file's repository IDs with /usr/bin/jq (0002 PLAN M3-4), installed here
# when the base image lacks it; the normal image does not need jq.
ARG HOOK_RECORD=0

RUN set -eux; \
    chown root:root /usr/local/bin/scaler-entrypoint /usr/local/bin/scaler-job-started.sh; \
    chmod 0755 /usr/local/bin/scaler-entrypoint /usr/local/bin/scaler-job-started.sh; \
    if [ "$HOOK_RECORD" = 1 ]; then \
        sed -i 's/^RECORD=0$/RECORD=1/' /usr/local/bin/scaler-job-started.sh; \
        grep -qx 'RECORD=1' /usr/local/bin/scaler-job-started.sh; \
        if [ ! -x /usr/bin/jq ]; then \
            apt-get update; \
            apt-get install -y --no-install-recommends jq; \
            rm -rf /var/lib/apt/lists/*; \
        fi; \
        /usr/bin/jq --version; \
    fi; \
    if command -v dockerd || command -v containerd; then exit 1; fi; \
    docker --version; \
    docker buildx version; \
    aws --version; \
    kubectl version --client; \
    git --version; \
    curl --version

USER runner
ENV ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/scaler-job-started.sh
ENTRYPOINT ["/usr/local/bin/scaler-entrypoint"]
CMD ["./run.sh"]
