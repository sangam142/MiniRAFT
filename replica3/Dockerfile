# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Copy source.
COPY . .

# Build.
RUN go build -o /app/replica .

# ── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM alpine:latest

RUN apk add --no-cache curl

COPY --from=builder /app/replica /app/replica

EXPOSE 9001 8081

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8081/health || exit 1

CMD ["/app/replica"]
