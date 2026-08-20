# Build the Rust server in a reproducible release image. The viewer is embedded
# in the binary, so the runtime only needs CA certificates and connector files.
FROM rust:1.90-bookworm AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
COPY web ./web
COPY connectors ./connectors
RUN cargo build --locked --release --bin oryxa

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/oryxa /oryxa

# Connectors are configuration; mount your own over this.
COPY connectors/templates /connectors/templates

EXPOSE 8080
ENTRYPOINT ["/oryxa"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--connectors", "/connectors"]
