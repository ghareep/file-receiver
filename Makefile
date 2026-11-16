BINARY  ?= file-receiver
GO      ?= go

.PHONY: all build test vet race install run clean

all: vet race build

build:
	$(GO) build -trimpath -o $(BINARY) .

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

install: build
	install -m 0755 $(BINARY) $(HOME)/.local/bin/$(BINARY)

run:
	$(GO) run .

clean:
	rm -f $(BINARY)
