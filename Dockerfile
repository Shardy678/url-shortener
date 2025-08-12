
# ---- builder ----
FROM golang:1.24.4-alpine AS builder
WORKDIR /app

# Cache deps
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build main package
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /app/app .

# ---- runtime ----
FROM alpine:3.20
WORKDIR /app

# Install runtime tools for debugging & health checks
RUN apk add --no-cache curl

COPY --from=builder /app/app /app/app

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/app"]

