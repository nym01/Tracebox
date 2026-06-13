FROM golang:1.25 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o goboxd ./cmd/tracebox

# Build nsjail from source (pinned to tag 3.4 via the external/nsjail submodule).
# Do NOT bundle a prebuilt binary and do NOT install nsjail from apt — it must
# be compiled at image-build time. This stage takes several minutes.
FROM debian:bookworm-slim AS nsjail-builder
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        autoconf bison flex gcc g++ git libprotobuf-dev \
        libnl-route-3-dev libtool make pkg-config protobuf-compiler && \
    rm -rf /var/lib/apt/lists/*
COPY external/nsjail /build/nsjail
RUN cd /build/nsjail && make && cp nsjail /usr/local/bin/nsjail

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        python3 g++ gcc bash nodejs default-jdk-headless iverilog \
        libprotobuf32 libnl-route-3-200 libnl-3-200 && \
    ln -sf /usr/bin/nodejs /usr/bin/node 2>/dev/null || true && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/goboxd .
COPY --from=nsjail-builder /usr/local/bin/nsjail /usr/local/bin/nsjail
COPY configs/ configs/
EXPOSE 8080
CMD ["./goboxd"]
