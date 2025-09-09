# -------- builder --------
FROM golang:1.24.4-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/shortener ./cmd/shortener

# -------- runtime (distroless) --------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder --chown=nonroot:nonroot /out/shortener /app/app
COPY --from=builder --chown=nonroot:nonroot /src/token_bucket.lua /app/token_bucket.lua

ENV PORT=8080 \
    RATE_LIMIT_LUA=/app/token_bucket.lua

EXPOSE 8080

ENTRYPOINT ["/app/app"]

