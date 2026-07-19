BINARY := ollama-broker
PKG := ./cmd/broker

.PHONY: build test race vet fmt clean

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) $(PKG)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf bin
