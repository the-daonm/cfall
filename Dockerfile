# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy dependency manifests
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary statically
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o gpu-fallback-webhook main.go

# Final stage
FROM alpine:3.20

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy the binary from the build stage
COPY --from=builder /app/gpu-fallback-webhook .

# Set running port and define entrypoint
EXPOSE 8443
ENTRYPOINT ["/app/gpu-fallback-webhook"]
