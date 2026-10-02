# EveSynapse — dev tasks

.PHONY: build run tidy gen clean

build: ## Compile the server into ./bin/evesynapse
	go build -o bin/evesynapse .

run: ## Run the server on :8080
	go run .

tidy: ## Sync go.mod/go.sum
	go mod tidy

gen: ## Regenerate sqlc query code (requires sqlc on PATH or in ~/bin)
	sqlc generate

clean: ## Remove build output (keeps the dev database)
	rm -rf bin
