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
