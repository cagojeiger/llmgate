FROM rust:1.96.1-bookworm AS builder
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release --locked

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /src/target/release/llmgate-caller /app/llmgate-caller
ENTRYPOINT ["/app/llmgate-caller"]
