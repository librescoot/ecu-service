.PHONY: build clean build-arm build-amd64 lint test

BINARY_NAME=ecu-service
BUILD_DIR=bin
LDFLAGS=-ldflags "-w -s -extldflags '-static'"

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

clean:
	rm -rf $(BUILD_DIR)

build-arm:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

build-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-amd64 .

lint:
	golangci-lint run

test:
	go test -v ./...
