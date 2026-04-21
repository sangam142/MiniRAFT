FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .
RUN go build -o gateway .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl

WORKDIR /app
COPY --from=builder /app/gateway .

EXPOSE 8080

HEALTHCHECK --interval=5s --timeout=3s --retries=5 \
  CMD curl -f http://localhost:8080/health || exit 1

CMD ["./gateway"]
