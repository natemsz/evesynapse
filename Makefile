# EveSynapse — dev tasks

.PHONY: build build-arm64 build-dev run tidy gen clean

build: ## Compile the release server into ./bin/evesynapse
	go build -o bin/evesynapse ./cmd/evesynapse

build-arm64: ## Cross-compile the release server for linux/arm64 (phone build)
	GOOS=linux GOARCH=arm64 go build -o bin/evesynapse-arm64 ./cmd/evesynapse

build-dev: ## Compile the dev server (includes /dev-login) into ./bin/evesynapse-dev
	go build -o bin/evesynapse-dev ./cmd/evesynapse-dev

run: ## Run the release server on :8080
	go run ./cmd/evesynapse

tidy: ## Sync go.mod/go.sum
	go mod tidy

gen: ## Regenerate sqlc query code (requires sqlc on PATH or in ~/bin)
	sqlc generate

clean: ## Remove build output (keeps the dev database)
	rm -rf bin
