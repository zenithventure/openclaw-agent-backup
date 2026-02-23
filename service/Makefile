.PHONY: build test run deploy clean

# Build for local dev
build:
	go build -o bin/backup-service .

# Build for Lambda (ARM64 Linux)
build-lambda:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bootstrap .

# Run locally (requires MinIO or S3 credentials)
run: build
	./bin/backup-service

# Run tests
test:
	go test -v -count=1 ./...

# Deploy to AWS via SAM
deploy: build-lambda
	sam deploy

# First-time deploy (interactive)
deploy-guided: build-lambda
	sam deploy --guided

# Validate SAM template
validate:
	sam validate

# Run locally via SAM (simulates Lambda + API Gateway)
sam-local: build-lambda
	sam local start-api

# Clean build artifacts
clean:
	rm -rf bin/ bootstrap .aws-sam/
