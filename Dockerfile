# -------- builder --------
FROM golang:1.24.4-alpine AS builder
WORKDIR /src

# Cache module downloads first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source (monorepo-friendly)
COPY . .

# Build the cmd/shortener binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w" \
    -o /out/shortener ./cmd/shortener

# (Optional) ensure CA bundle exists in builder to copy to runtime
# If this fails in your environment, you can remove it — the file often already exists.
RUN apk add --no-cache ca-certificates || true

# -------- runtime --------
FROM alpine:3.20
WORKDIR /app

# Copy binary and Lua script
COPY --from=builder /out/shortener /app/app
COPY --from=builder /src/token_bucket.lua /app/token_bucket.lua

# Copy CA bundle from builder instead of installing via apk (avoids repo outages)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Create non-root user using busybox tools (no apk needed)
RUN addgroup -S app && adduser -S app -G app

# Default envs
ENV PORT=8080 \
    RATE_LIMIT_LUA=/app/token_bucket.lua

EXPOSE 8080

# Healthcheck using busybox wget (built into Alpine)
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO- http://localhost:${PORT}/healthz >/dev/null 2>&1 || exit 1

USER app
ENTRYPOINT ["/app/app"]
