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

EXT_UUID := claude-usage@huylee15.github.io
EXT_SRC  := gnome-extension/$(EXT_UUID)
EXT_DIR  := $(HOME)/.local/share/gnome-shell/extensions/$(EXT_UUID)

.PHONY: install-extension uninstall-extension

## install-extension: install and enable the GNOME Shell panel indicator
install-extension:
	@mkdir -p $(EXT_DIR)
	install -m 0644 $(EXT_SRC)/metadata.json $(EXT_SRC)/extension.js $(EXT_SRC)/stylesheet.css $(EXT_DIR)/
	@sh gnome-extension/enable.sh $(EXT_UUID)
	@echo "installed $(EXT_DIR)"
	@echo "reload GNOME Shell once: X11 -> Alt+F2, r, Enter; Wayland -> log out and back in"

## uninstall-extension: disable and remove the GNOME Shell panel indicator
uninstall-extension:
	-gnome-extensions disable $(EXT_UUID)
	rm -rf $(EXT_DIR)
