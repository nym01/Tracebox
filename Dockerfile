FROM golang:1.22 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o goboxd ./cmd/goboxd

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends python3 g++ gcc bash && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/goboxd .
COPY configs/ configs/
EXPOSE 8080
CMD ["./goboxd"]
