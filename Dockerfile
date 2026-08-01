FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.9.0 AS xx

# hadolint global ignore=DL3018
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /app

# Install build dependencies
# build-base for CGo
# ca-certificates for HTTPS/GitHub API calls
# libwebp-dev for headers (needed at build time)
# libwebp-static for static linking
# git for go mod download
RUN apk add --no-cache git clang

# Copy go mod and sum files for dependency caching
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY --from=xx / /

ARG TARGETPLATFORM
RUN xx-apk add --no-cache gcc g++ libwebp-dev libwebp-static

# Development Stage - Hot Reloading
FROM builder AS dev
CMD ["go", "tool", "air"]

# Production Build Stage
FROM builder AS build-production

# Copy source code
COPY . .

# Version Info - Keep these ARGs for build-time injection
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Build all Go binaries in a single layer
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -x \
    && CGO_ENABLED=1 xx-go build \
    -ldflags="-w -s -extldflags '-static' -X 'tronbyt-server/internal/version.Version=${VERSION}' -X 'tronbyt-server/internal/version.Commit=${COMMIT}' -X 'tronbyt-server/internal/version.BuildDate=${BUILD_DATE}'" \
    -tags gzip_fonts \
    -o build/app/tronbyt-server \
    ./cmd/server

WORKDIR /app/build

RUN ln -s /app/tronbyt-server boot \
    && ln -s /app/tronbyt-server app/migrate

# --- Runtime Stage ---
FROM scratch AS production

WORKDIR /app

# Copy CA certificates from builder so TLS works in the scratch image
COPY --from=build-production /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Copy compiled binaries from builder
COPY --from=build-production /app/build /

# Expose port
EXPOSE 8000

# Use the Go-based entrypoint wrapper
ENTRYPOINT ["/boot"]

# Default environment variables
ENV DB_DSN=data/tronbyt.db
ENV DATA_DIR=data

# Default command to execute the main server binary
CMD ["/app/tronbyt-server"]

# Rehearsal-only utility image. This target is separate from production so
# local seed, backup, poll and load tooling is never shipped in the server
# runtime image.
FROM build-production AS build-rehearsal
WORKDIR /app
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 xx-go build \
    -tags gzip_fonts \
    -o build/app/tronbyt-rehearsal \
    ./cmd/rehearsal

FROM scratch AS rehearsal
WORKDIR /app
COPY --from=build-rehearsal /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build-rehearsal /app/build/app/tronbyt-rehearsal /app/tronbyt-rehearsal
ENTRYPOINT ["/app/tronbyt-rehearsal"]

# Preserve the historical default `docker build .` result.
FROM production AS final
