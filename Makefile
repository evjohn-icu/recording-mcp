.PHONY: build test

build:
	go build -o bin/recording-mcp ./cmd/server
	go build -o bin/context-memory ./cmd/context-memory
	go build -o bin/import-conversation ./cmd/import-conversation
	go build -o bin/adapter-doubao ./cmd/adapter-doubao
	go build -o bin/import-transcript ./cmd/import-transcript
	go build -o bin/context-worker ./cmd/context-worker

test:
	go test -race ./...
