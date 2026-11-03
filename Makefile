# Makefile for the scaleway-public FinFocus plugin.

.PHONY: help build test cover lint fmt install clean

PLUGIN_NAME = scaleway-public
BINARY_NAME = finfocus-plugin-$(PLUGIN_NAME)
BUILD_DIR = bin
CMD_DIR = cmd/plugin
# The latest release tag without its v, or 0.1.0 before the first release.
VERSION ?= $(or $(patsubst v%,%,$(shell git describe --tags --abbrev=0 2>/dev/null)),0.1.0)
LDFLAGS = -X github.com/mkutlak/finfocus-plugin-scaleway-public/internal/pricing.PluginVersion=$(VERSION)
# Same plugin directory FinFocus discovery reads.
PLUGIN_DIR ?= $(or $(FINFOCUS_PLUGIN_DIR),$(if $(FINFOCUS_HOME),$(FINFOCUS_HOME)/plugins,$(HOME)/.finfocus/plugins))
INSTALL_DIR = $(PLUGIN_DIR)/$(PLUGIN_NAME)/v$(VERSION)

help:
	@echo "build    - Build bin/$(BINARY_NAME)"
	@echo "test     - Run unit tests with -race"
	@echo "cover    - Write coverage.html"
	@echo "lint     - Run golangci-lint"
	@echo "fmt      - Format code"
	@echo "install  - Install into $(INSTALL_DIR)"
	@echo "clean    - Remove build output"

build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run --config .golangci-lint.yml --allow-parallel-runners

fmt:
	go fmt ./...

install: build
	@mkdir -p $(INSTALL_DIR)
	cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/
	@printf '{\n  "name": "%s",\n  "version": "%s"\n}\n' "$(PLUGIN_NAME)" "v$(VERSION)" \
		> $(INSTALL_DIR)/plugin.manifest.json
	@echo "Installed to $(INSTALL_DIR)"

clean:
	rm -rf $(BUILD_DIR) dist coverage.out coverage.html
