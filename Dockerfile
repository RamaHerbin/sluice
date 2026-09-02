# syntax=docker/dockerfile:1

# Stage 1: build a static binary. Module files are copied first so the
# download layer is cached until go.mod or go.sum change.
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sluice ./cmd/sluice

# Stage 2: distroless static image, non-root, nothing but the binary.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /out/sluice /sluice
USER nonroot:nonroot
# 8080 proxy, 9090 admin (/healthz /readyz /metrics)
EXPOSE 8080 9090
ENTRYPOINT ["/sluice"]
