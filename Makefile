.PHONY: build install uninstall clean run doctor fmt vet

PREFIX ?= $(HOME)/.local
BIN    := $(PREFIX)/bin
APP    := qlaude

build:
	go build -o $(APP) ./cmd/qlaude

install: build
	mkdir -p $(BIN)
	install -m 0755 $(APP) $(BIN)/$(APP)
	@echo "installed $(BIN)/$(APP)"
	@command -v $(APP) >/dev/null 2>&1 || echo "note: add $(BIN) to your PATH"

uninstall:
	rm -f $(BIN)/$(APP)

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(APP)

run: build
	./$(APP)

doctor: build
	./$(APP) --qlaude doctor
