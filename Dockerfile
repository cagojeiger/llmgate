# Multi-stage build: alpine builder for go modules, distroless static
# for the runtime image. The binary is fully static (CGO_ENABLED=0)
# so distroless/static is sufficient — no libc needed.

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/llmgate ./cmd/llmgate

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/llmgate /app/llmgate
# Ship a default catalog inside the image so `docker run` works zero-config.
# Prod always overrides via LLMGATE_CATALOG to a ConfigMap mount, so this
# embed is effectively a dev/standalone fallback — never the source of
# truth for any deployed environment.
COPY --from=builder /src/catalog /app/catalog
EXPOSE 8080
ENTRYPOINT ["/app/llmgate"]
