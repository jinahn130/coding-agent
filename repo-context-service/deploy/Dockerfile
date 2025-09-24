# Multi-stage build for optimized production image

# Build stage
FROM golang:1.23-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    protobuf \
    ca-certificates

# Install Go protobuf tools
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest && \
    go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Generate protobuf files
RUN make proto

# Build the application
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o bin/apiserver \
    ./cmd/apiserver

# Production stage
FROM alpine:3.18

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    git \
    ripgrep \
    tzdata \
    && update-ca-certificates

# Create non-root user
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/bin/apiserver /app/apiserver

# Copy static files
COPY --from=builder /app/ui/web /app/ui/web

# Create necessary directories
RUN mkdir -p /app/data/repositories /app/data/temp && \
    chown -R appuser:appgroup /app

# Switch to non-root user
USER appuser

# Expose ports
EXPOSE 8080 9090 8081

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Set default environment variables
ENV ENVIRONMENT=production \
    LOG_LEVEL=info \
    HTTP_PORT=8080 \
    GRPC_PORT=9090 \
    ADMIN_PORT=8081 \
    UPLOAD_STORAGE_DIR=/app/data/repositories \
    UPLOAD_TEMP_DIR=/app/data/temp

# Run the application
CMD ["/app/apiserver"]