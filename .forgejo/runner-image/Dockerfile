# The prepared image for the laptop runner: node:22-bookworm with the
# release workflow's toolbox and Go already installed, so release jobs
# start with the items they need to build instead of installing them
# on every run. The runner's ubuntu-latest label points at this image.
#
# Build it on the runner host:
#   docker build -t evesynapse-runner:latest .forgejo/runner-image
FROM node:22-bookworm

# The toolbox the release workflow installs per job today.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        curl \
        ca-certificates \
        git \
        make \
        python3 \
    && rm -rf /var/lib/apt/lists/*

# Go 1.27.3, the version the release workflow pins, downloaded for
# the build architecture and verified against the same SHA-256 sums
# the workflow verifies.
ARG TARGETARCH
RUN case "${TARGETARCH:-$(uname -m)}" in \
        arm64|aarch64) go_arch=arm64; go_sum="a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49" ;; \
        *) go_arch=amd64; go_sum="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40" ;; \
    esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go1.27.3.linux-${go_arch}.tar.gz" \
    && echo "$go_sum  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/go.tar.gz -C /usr/local \
    && rm /tmp/go.tar.gz \
    && /usr/local/go/bin/go version

ENV PATH="/usr/local/go/bin:${PATH}"
