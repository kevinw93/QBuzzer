NPM ?= npm
BIN := bin/qbuzzer

.PHONY: dev gen check-gen test lint build run clean

web/node_modules: web/package.json web/package-lock.json
	cd web && $(NPM) ci
	@touch web/node_modules

# Go server on 127.0.0.1:8080; Vite on localhost:5173 proxying /api to it.
dev: web/node_modules
	@trap 'kill 0' INT TERM EXIT; \
	go run ./cmd/server & \
	cd web && $(NPM) run dev

gen:
	go tool tygo generate

# Fails if web/src/protocol.ts is stale relative to internal/protocol.
check-gen: gen
	@git diff --exit-code -- web/src/protocol.ts || \
		(echo "web/src/protocol.ts is stale: run 'make gen' and commit" && exit 1)

test:
	go test -race ./...

lint: web/node_modules
	golangci-lint run ./...
	cd web && $(NPM) run lint

# The frontend must be built first: the Go binary embeds web/dist.
build: web/node_modules
	cd web && $(NPM) run build
	@touch web/dist/.gitkeep
	go build -o $(BIN) ./cmd/server

run: build
	./$(BIN)

clean:
	rm -rf bin web/dist/assets web/dist/*.html web/dist/*.svg
