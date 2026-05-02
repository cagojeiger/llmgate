# Multi-stage build: alpine builder for go modules, distroless static
# for the runtime image. The binary is fully static (CGO_ENABLED=0)
# so distroless/static is sufficient — no libc needed.

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmgate ./cmd/llmgate

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/llmgate /app/llmgate
EXPOSE 8080
ENTRYPOINT ["/app/llmgate"]
