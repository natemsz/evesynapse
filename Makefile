# EveSynapse — dev tasks

.PHONY: build build-arm64 build-amd64 build-dev run tidy gen clean assets

build: ## Compile the release server into ./bin/evesynapse
	go build -o bin/evesynapse ./cmd/evesynapse

build-arm64: ## Cross-compile the release server for linux/arm64 (ARM servers)
	GOOS=linux GOARCH=arm64 go build -o bin/evesynapse-arm64 ./cmd/evesynapse

build-amd64: ## Cross-compile the release server for linux/amd64 (Intel/AMD servers)
	GOOS=linux GOARCH=amd64 go build -o bin/evesynapse-amd64 ./cmd/evesynapse

assets: ## Decode the fonts/wallpaper from ci-assets/ into the embedded asset dir
	mkdir -p internal/app/static/fonts
	for f in ci-assets/fonts/*.b64; do base64 -d "$$f" > "internal/app/static/fonts/$$(basename "$${f%.b64}")"; done
	cat ci-assets/bg.jpg.b64.part-* | base64 -d > internal/app/static/bg.jpg

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
