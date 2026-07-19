VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ikaro1192/batchbru.Version=$(VERSION)

.PHONY: build test lint clean install

build:
	go build -ldflags "$(LDFLAGS)" -o batchbru ./cmd/batchbru

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/batchbru

test:
	go test -race ./...

lint:
	go vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; echo "gofmt: the files above are not formatted"; exit 1; fi

clean:
	rm -f batchbru
