BINARY := claude-session-guard
PREFIX ?= $(HOME)/.local
BIN_DIR := $(PREFIX)/bin

.PHONY: build install uninstall clean test fmt vet

build:
	go build -trimpath -ldflags '-s -w' -o $(BINARY) ./cmd/$(BINARY)

install: build
	install -d $(BIN_DIR)
	install -m 0755 $(BINARY) $(BIN_DIR)/$(BINARY)
	@echo "installed to $(BIN_DIR)/$(BINARY)"

uninstall:
	rm -f $(BIN_DIR)/$(BINARY)

clean:
	rm -f $(BINARY)

test:
	go test ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...
