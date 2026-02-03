.PHONY: build clean run test install

BINARY_NAME=tailgater
MAIN_PATH=./cmd/tailgater
BUILD_DIR=.

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)

clean:
	rm -f $(BUILD_DIR)/$(BINARY_NAME)
	go clean

run: build
	./$(BINARY_NAME)

run-web: build
	./$(BINARY_NAME) -web

test:
	go test -v ./...

install: build
	go install $(MAIN_PATH)

deps:
	go mod download
	go mod tidy

# Cross-compilation
build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PATH)

build-darwin:
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(MAIN_PATH)
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PATH)

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(MAIN_PATH)

build-all: build-linux build-darwin build-windows
