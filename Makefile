# Build the xAI OAuth plugin as a CLIProxyAPI dynamic plugin.
#
# Output: bin/xai-oauth-v<version>.<ext> ready to drop into the server's
# plugins directory (plugins/<goos>/<goarch>/ or the plugins dir directly).

VERSION := 0.1.0
PLUGIN_ID := xai-oauth

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

ifeq ($(OS),Windows_NT)
PLUGIN_EXT := dll
else ifeq ($(UNAME_S),Darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

GOOS := $(shell uname -s | tr 'A-Z' 'a-z')
GOARCH := $(UNAME_M)
ifeq ($(GOARCH),aarch64)
GOARCH := arm64
endif
ifeq ($(OS),Windows_NT)
GOOS := windows
ifeq ($(GOARCH),AMD64)
GOARCH := amd64
endif
endif

BIN_DIR := $(CURDIR)/bin
OUT := $(BIN_DIR)/$(PLUGIN_ID)-v$(VERSION).$(PLUGIN_EXT)

# Point this at the plugins directory of your CLIProxyAPI deployment
# (the directory referenced by `plugins.dir` in config.yaml, default "plugins").
PLUGINS_DIR ?=

.PHONY: build test vet fmt clean install

build: vet test $(OUT)

$(OUT): $(wildcard *.go catalog.json go.mod)
	@mkdir -p $(BIN_DIR)
	go build -buildmode=c-shared -o $(abspath $(OUT)) .
	rm -f $(BIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h
	@echo "built $(OUT)"

vet:
	go vet ./...

test:
	go test ./...

fmt:
	gofmt -w .

# Install the built plugin into a CLIProxyAPI plugins directory.
# Usage: make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
# Without PLUGINS_DIR the default "plugins" directory of the working
# directory is used (matching the CLIProxyAPI default `plugins.dir`).
install: build
	@if [ -z "$(PLUGINS_DIR)" ]; then \
		echo "PLUGINS_DIR is empty; refusing to guess. Example:"; \
		echo "  make install PLUGINS_DIR=/path/to/cliproxyapi/plugins"; \
		exit 1; \
	fi
	@mkdir -p $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)
	cp $(abspath $(OUT)) $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/
	@echo "installed into $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/"

clean:
	rm -rf $(BIN_DIR)
