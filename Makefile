BINARY_NAME := ecu-service
BUILD_DIR   := bin
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS     := -ldflags "-w -s -X main.version=$(VERSION)"

build:
	mkdir -p $(BUILD_DIR)
	GOTOOLCHAIN=go1.25.7 CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	GOTOOLCHAIN=go1.25.7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

dist: build

test:
	GOTOOLCHAIN=go1.25.7 go test ./...

lint:
	golangci-lint run

fmt:
	go fmt ./...

deps:
	GOTOOLCHAIN=go1.25.7 go mod download && go mod tidy

clean:
	rm -rf $(BUILD_DIR)

.PHONY: build build-arm build-host dist test lint fmt deps clean
