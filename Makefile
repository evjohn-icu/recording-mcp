.PHONY: build test demo e2e-cloud web web-demo spec-validate python-test ci

build:
	go build -o bin/recording-mcp ./cmd/server
	go build -o bin/context-memory ./cmd/context-memory
	go build -o bin/context-web ./cmd/context-web
	go build -o bin/context-profile ./cmd/context-profile
	go build -o bin/context-worldline ./cmd/context-worldline
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

web:
	cd web && npm ci && npm run build

test:
	go test -race ./...

demo:
	sh scripts/demo.sh

e2e-cloud:
	sh scripts/e2e-cloud.sh

# Builds the frontend and binaries, then starts a disposable synthetic demo.
# PORT may be overridden, for example: make web-demo PORT=8790.
web-demo: build web
	sh scripts/web-demo.sh $(PORT)

# OpenSpec validates artifact structure; the test map checks that scenario IDs
# point to existing automated checks. Neither proves tests were written first.
spec-validate:
	npx openspec validate --all
	python3 scripts/check_spec_test_map.py

python-test:
	python3 -m unittest discover -s scripts -p 'test_*.py' -v

ci: spec-validate python-test
	go vet ./...
	go test -race -count=1 ./...
	make build
