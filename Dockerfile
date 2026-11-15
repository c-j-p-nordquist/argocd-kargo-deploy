# syntax=docker/dockerfile:1

# Base images are pinned by digest. Update the tag and digest together.
FROM golang:1.26-alpine@sha256:30b1e546a9c7fa3b747c2bdba3a0c7bdf3a02ebb4c5b8405d2f948d7e8aac4ec AS base
WORKDIR /app

FROM base AS deps
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# dev: full toolchain and source, used by compose for tests and local runs.
FROM deps AS dev
COPY . .
CMD ["go", "test", "./..."]

# tools: the dev toolchain plus the supply chain tools used to generate and
# check components.lock. Not part of the release image.
FROM gcr.io/go-containerregistry/crane:v0.22.1@sha256:1f968817b95790bed063f71175aa6b8ff879fa17064020415f3e18bb6e6a36e1 AS crane
FROM ghcr.io/sigstore/cosign/cosign:v3.1.4@sha256:0f06154836d2de34f887a5c273506d52ca43edaa1d9e53e6510fa78a48f69c2d AS cosign

FROM base AS helm-amd64
ADD --checksum=sha256:0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b \
    https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz /tmp/helm.tar.gz

FROM base AS helm-arm64
ADD --checksum=sha256:8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88 \
    https://get.helm.sh/helm-v4.3.1-linux-arm64.tar.gz /tmp/helm.tar.gz

FROM helm-${TARGETARCH} AS helm
RUN tar -xzf /tmp/helm.tar.gz -C /tmp --strip-components=1 && install /tmp/helm /usr/local/bin/helm

FROM deps AS tools
COPY --from=crane /ko-app/crane /usr/local/bin/crane
COPY --from=cosign /ko-app/cosign /usr/local/bin/cosign
COPY --from=helm /usr/local/bin/helm /usr/local/bin/helm
COPY . .

FROM deps AS build
ARG VERSION=dev
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/cli.Version=${VERSION}" \
      -o /bin/akd ./cmd/akd

# prod: static binary, no shell, non-root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS prod
COPY --from=build /bin/akd /akd
ENTRYPOINT ["/akd"]
