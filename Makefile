BINARY      := claude-usage
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build install vet fmt clean

## build: compile into ./bin/claude-usage
build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags '-X main.version=$(VERSION)' -o $(BIN_DIR)/$(BINARY) .

## install: copy the binary to $(INSTALL_DIR) (default ~/.local/bin)
install: build
	@mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BIN_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "installed $(INSTALL_DIR)/$(BINARY)"

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BIN_DIR)
