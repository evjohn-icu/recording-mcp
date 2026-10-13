.PHONY: build test demo e2e-cloud

build:
	go build -o bin/recording-mcp ./cmd/server
	go build -o bin/context-memory ./cmd/context-memory
	go build -o bin/import-conversation ./cmd/import-conversation
	go build -o bin/adapter-doubao ./cmd/adapter-doubao
	go build -o bin/asr-ingest ./cmd/asr-ingest
	go build -o bin/import-transcript ./cmd/import-transcript
	go build -o bin/context-worker ./cmd/context-worker
	go build -o bin/context-associate ./cmd/context-associate
	go build -o bin/context-decision ./cmd/context-decision
	go build -o bin/context-eval ./cmd/context-eval
	go build -o bin/context-embed ./cmd/context-embed
	go build -o bin/context-sync ./cmd/context-sync
	go build -o bin/record-sim ./cmd/record-sim

test:
	go test -race ./...

.PHONY: demo
demo:
	sh scripts/demo.sh

.PHONY: e2e-cloud
e2e-cloud:
	sh scripts/e2e-cloud.sh
