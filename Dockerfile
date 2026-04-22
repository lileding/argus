# Build stage: compile with musl for fully static binary.
FROM rust:1-alpine AS builder

RUN apk add --no-cache musl-dev pkgconfig openssl-dev openssl-libs-static perl make

WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY feishu/ feishu/
COPY src/ src/
COPY migrations/ migrations/

RUN cargo build --release && \
    strip target/release/argus

# Runtime stage: minimal alpine with CLI tools.
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    bash \
    curl \
    jq \
    git \
    poppler-utils \
    python3

RUN addgroup -S argus && adduser -S argus -G argus

COPY --from=builder /src/target/release/argus /usr/local/bin/argus

WORKDIR /app
RUN chown argus:argus /app
USER argus

ENTRYPOINT ["argus"]
CMD ["--config", "/app/config.toml"]
