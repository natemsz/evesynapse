# The prepared image for the laptop runner: node:22-bookworm with the
# release workflow's toolbox and Go already installed, so release jobs
# start with the items they need to build instead of installing them
# on every run. The runner's ubuntu-latest label points at this image.
#
# Build it on the runner host:
#   docker build -t evesynapse-runner:latest .forgejo/runner-image
FROM node:26-bookworm

# The toolbox the release workflow installs per job today.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        curl \
        ca-certificates \
        git \
        make \
        python3 \
    && rm -rf /var/lib/apt/lists/*

# Go 1.27.4, the version the release workflow pins, downloaded for
# the build architecture and verified against the same SHA-256 sums
# the workflow verifies.
ARG TARGETARCH
RUN case "${TARGETARCH:-$(uname -m)}" in \
        arm64|aarch64) go_arch=arm64; go_sum="3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92" ;; \
        *) go_arch=amd64; go_sum="7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840" ;; \
    esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go1.27.4.linux-${go_arch}.tar.gz" \
    && echo "$go_sum  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/go.tar.gz -C /usr/local \
    && rm /tmp/go.tar.gz \
    && /usr/local/go/bin/go version

ENV PATH="/usr/local/go/bin:${PATH}"
