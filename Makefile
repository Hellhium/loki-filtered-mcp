.PHONY: all build test vet fmt fmt-check tidy run clean

BINARY := loki-filtered-mcp
CONFIG  ?= config.example.yaml

all: fmt-check vet test build

build:
	go build -o $(BINARY) ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

run: build
	./$(BINARY) -config $(CONFIG)

clean:
	rm -f $(BINARY)
