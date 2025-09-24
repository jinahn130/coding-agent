.PHONY: proto generate build run test lint clean docker docker-compose up up-fresh down setup deps install-tools

# Go settings
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
CGO_ENABLED ?= 0

# Build settings
BINARY_NAME = apiserver
BUILD_DIR = ./build
CMD_DIR = ./cmd/apiserver
PROTO_DIR = ./proto
PROTO_GEN_DIR = ./proto/gen

# Docker settings
DOCKER_IMAGE = repo-context-service
DOCKER_TAG ?= latest

# Setup target - run this first!
setup: install-tools deps proto
	@echo "Setup complete! You can now run 'make build' or 'make up'"

# Install required development tools
install-tools:
	@echo "Installing development tools..."
	@command -v protoc >/dev/null 2>&1 || { echo "protoc not found. Please install Protocol Buffers compiler"; exit 1; }
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@go install golang.org/x/tools/cmd/goimports@latest

# Download and tidy dependencies
deps:
	@echo "Downloading dependencies..."
	@go mod download
	@go mod tidy

# Proto generation
proto: clean-proto
	@echo "Generating protobuf code..."
	@mkdir -p $(PROTO_GEN_DIR)
	@mkdir -p $(PROTO_GEN_DIR)/repocontext/v1
	@protoc \
		--proto_path=$(PROTO_DIR) \
		--proto_path=third_party \
		--go_out=$(PROTO_GEN_DIR)/repocontext/v1 \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_GEN_DIR)/repocontext/v1 \
		--go-grpc_opt=paths=source_relative \
		--grpc-gateway_out=$(PROTO_GEN_DIR)/repocontext/v1 \
		--grpc-gateway_opt=paths=source_relative \
		--grpc-gateway_opt=generate_unbound_methods=true \
		$(PROTO_DIR)/*.proto

generate: proto
	@echo "Running go generate..."
	@go generate ./...

build: generate
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

run: build
	@echo "Running $(BINARY_NAME)..."
	@$(BUILD_DIR)/$(BINARY_NAME)

dev: generate
	@echo "Running in development mode..."
	@go run $(CMD_DIR)/main.go

test:
	@echo "Running tests..."
	@go test -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html

lint:
	@echo "Running linters..."
	@golangci-lint run ./...

fmt:
	@echo "Formatting code..."
	@go fmt ./...
	@goimports -w .

clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out coverage.html

clean-proto:
	@echo "Cleaning generated proto files..."
	@rm -rf $(PROTO_GEN_DIR)

# Docker commands
docker:
	@echo "Building Docker image..."
	@docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

docker-run: docker
	@echo "Running Docker container..."
	@docker run --rm -p 8080:8080 -p 9090:9090 \
		--env-file .env \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

# Docker Compose commands
up:
	@echo "Starting services with docker-compose..."
	@if [ -z "$(OPENAI_API_KEY)" ]; then \
		echo "Error: OPENAI_API_KEY environment variable is required"; \
		echo "Please run: export OPENAI_API_KEY=your-key-here"; \
		exit 1; \
	fi
	@if [ -z "$(DEEPSEEK_API_KEY)" ]; then \
		echo "Error: DEEPSEEK_API_KEY environment variable is required"; \
		echo "Please run: export DEEPSEEK_API_KEY=your-key-here"; \
		exit 1; \
	fi
	@OPENAI_API_KEY=$(OPENAI_API_KEY) DEEPSEEK_API_KEY=$(DEEPSEEK_API_KEY) \
		docker-compose -f deploy/docker-compose.dev.yml up --build

# Force rebuild all Docker images (use when code changes don't appear)
up-fresh:
	@echo "🔄 Force rebuilding all Docker images and starting services..."
	@if [ -z "$(OPENAI_API_KEY)" ]; then \
		echo "Error: OPENAI_API_KEY environment variable is required"; \
		echo "Please run: export OPENAI_API_KEY=your-key-here"; \
		exit 1; \
	fi
	@if [ -z "$(DEEPSEEK_API_KEY)" ]; then \
		echo "Error: DEEPSEEK_API_KEY environment variable is required"; \
		echo "Please run: export DEEPSEEK_API_KEY=your-key-here"; \
		exit 1; \
	fi
	@echo "⚠️  This will rebuild all Docker images from scratch (may take 5-10 minutes)"
	@OPENAI_API_KEY=$(OPENAI_API_KEY) DEEPSEEK_API_KEY=$(DEEPSEEK_API_KEY) \
		docker-compose -f deploy/docker-compose.dev.yml build --no-cache
	@OPENAI_API_KEY=$(OPENAI_API_KEY) DEEPSEEK_API_KEY=$(DEEPSEEK_API_KEY) \
		docker-compose -f deploy/docker-compose.dev.yml up

down:
	@echo "Stopping services..."
	@docker-compose -f deploy/docker-compose.dev.yml down

logs:
	@echo "Showing logs..."
	@docker-compose -f deploy/docker-compose.dev.yml logs -f

# Development helpers

# Health checks for local development
health:
	@echo "Checking service health..."
	@curl -f http://localhost:8080/health || echo "Service not responding"

grpc-health:
	@echo "Checking gRPC health..."
	@grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check || echo "gRPC service not responding"

# Example API calls
example-upload:
	@echo "Example: Upload a repository..."
	@curl -X POST http://localhost:8080/v1/upload \
		-H "Content-Type: application/json" \
		-d '{"git_repository": {"url": "https://github.com/example/repo.git", "ref": "main"}}'

example-chat:
	@echo "Example: Chat with repository..."
	@curl -X POST http://localhost:8080/v1/chat/repo-123/stream \
		-H "Content-Type: application/json" \
		-d '{"start": {"repository_id": "repo-123", "tenant_id": "local"}}'

help:
	@echo "Available targets:"
	@echo "  setup         - 🚀 FIRST TIME: Install tools, deps, and generate proto files"
	@echo "  proto         - Generate protobuf code"
	@echo "  generate      - Run go generate and proto generation"
	@echo "  build         - Build the binary"
	@echo "  run           - Build and run the binary"
	@echo "  dev           - Run in development mode"
	@echo "  test          - Run tests with coverage"
	@echo "  lint          - Run linters"
	@echo "  fmt           - Format code"
	@echo "  clean         - Clean build artifacts"
	@echo "  docker        - Build Docker image"
	@echo "  up            - Start all services with docker-compose"
	@echo "  up-fresh      - 🔄 Force rebuild all images (use when code changes don't appear)"
	@echo "  down          - Stop all services"
	@echo "  install-tools - Install development tools"
	@echo "  deps          - Download and tidy dependencies"
	@echo "  health        - Check service health"
	@echo "  help          - Show this help"