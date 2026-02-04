APP_NAME       := s0ultilz-bot
BUILD_DIR      := dist
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(APP_NAME) .

.PHONY: build-linux
build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(APP_NAME)_linux_amd64 .

.PHONY: build-mac-amd
build-mac-amd:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(APP_NAME)_darwin_amd64 .

.PHONY: build-mac-arm
build-mac-arm:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(APP_NAME)_darwin_arm64 .

.PHONY: release
release: build-linux build-mac-amd build-mac-arm

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR)
