APP_NAME := vps-setup
DIST_DIR := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X main.Version=$(VERSION)

.PHONY: all build-linux build-arm64 release test clean

all: build-linux

build-linux:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(APP_NAME)-linux-amd64 .

build-arm64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(APP_NAME)-linux-arm64 .

release: clean test build-linux build-arm64
	@cd $(DIST_DIR) && sha256sum $(APP_NAME)-linux-* > checksums.txt
	@echo "✓ Релиз $(VERSION) собран в ./$(DIST_DIR)/"

test:
	go vet ./...
	go test -v -race ./...

clean:
	rm -rf $(DIST_DIR)
