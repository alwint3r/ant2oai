.PHONY: build fmt lint test

build:
	mkdir -p bin
	go build -trimpath -o bin/ant2oai .

fmt:
	gofmt -w *.go

lint:
	@test -z "$$(gofmt -l *.go)" || (echo 'Run make fmt'; exit 1)
	go vet ./...

test:
	go test ./...
