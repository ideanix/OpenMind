VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet run docker

build:
	go build -ldflags '$(LDFLAGS)' -o bin/openmind ./cmd/openmind

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/openmind serve

docker:
	docker build -t openmind:$(VERSION) .
