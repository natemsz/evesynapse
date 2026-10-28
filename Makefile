# EveSynapse — dev tasks

.PHONY: modules build build-arm64 build-amd64 build-dev run tidy gen clean assets test fmt check

build: ## Compile the release server into ./bin/evesynapse
	go build -o bin/evesynapse ./cmd/evesynapse

modules: ## List the optional modules; choose some with: go run ./cmd/modbuild -modules a,b -write
	go run ./cmd/modbuild -list

test: ## Run the test suite (it starts its own embedded PostgreSQL)
	go test ./...

fmt: ## Format the Go sources
	gofmt -w cmd internal

check: ## Everything CI checks: formatting, vet, and the tests under the race detector
	@unformatted="$$(gofmt -l cmd internal)"; if [ -n "$$unformatted" ]; then echo "These files are not gofmt-formatted (run: make fmt):"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

build-arm64: ## Cross-compile the release server for linux/arm64 (ARM servers)
	GOOS=linux GOARCH=arm64 go build -o bin/evesynapse-arm64 ./cmd/evesynapse

build-amd64: ## Cross-compile the release server for linux/amd64 (Intel/AMD servers)
	GOOS=linux GOARCH=amd64 go build -o bin/evesynapse-amd64 ./cmd/evesynapse

assets: ## Decode the fonts/wallpaper from ci-assets/ into the embedded asset dir
	mkdir -p internal/app/static/fonts
	for f in ci-assets/fonts/*.b64; do base64 -d "$$f" > "internal/app/static/fonts/$$(basename "$${f%.b64}")"; done
	ls ci-assets/bg.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg.jpg
	ls ci-assets/bg-thumb.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-thumb.jpg
	ls ci-assets/bg-4k.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-4k.jpg
	ls ci-assets/bg-uhd.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-uhd.jpg
	ls ci-assets/bg.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg.jxl
	ls ci-assets/bg-4k.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-4k.jxl
	ls ci-assets/bg-uhd.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-uhd.jxl
	ls ci-assets/bg-calm.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm.jpg
	ls ci-assets/bg-calm-thumb.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-thumb.jpg
	ls ci-assets/bg-calm-4k.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-4k.jpg
	ls ci-assets/bg-calm-uhd.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-uhd.jpg
	ls ci-assets/bg-calm.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm.jxl
	ls ci-assets/bg-calm-4k.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-4k.jxl
	ls ci-assets/bg-calm-uhd.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-uhd.jxl
	ls ci-assets/bg-calm-muted.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted.jpg
	ls ci-assets/bg-calm-muted-thumb.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted-thumb.jpg
	ls ci-assets/bg-calm-muted-4k.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted-4k.jpg
	ls ci-assets/bg-calm-muted-uhd.jpg.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted-uhd.jpg
	ls ci-assets/bg-calm-muted.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted.jxl
	ls ci-assets/bg-calm-muted-4k.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted-4k.jxl
	ls ci-assets/bg-calm-muted-uhd.jxl.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/bg-calm-muted-uhd.jxl
	ls ci-assets/shot-fitting.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-fitting.webp
	ls ci-assets/shot-planner.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-planner.webp
	ls ci-assets/shot-overview.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-overview.webp
	ls ci-assets/shot-ideas.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-ideas.webp
	ls ci-assets/shot-market.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-market.webp
	ls ci-assets/shot-threat.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-threat.webp
	ls ci-assets/shot-map.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-map.webp
	ls ci-assets/shot-intel.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-intel.webp
	ls ci-assets/shot-simulator.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-simulator.webp
	ls ci-assets/shot-doctrines.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-doctrines.webp
	ls ci-assets/shot-appraisal.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-appraisal.webp
	ls ci-assets/shot-axon.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/shot-axon.webp
	ls ci-assets/stars-tile.webp.b64.part-* | sort -V | xargs cat | base64 -d > internal/app/static/stars-tile.webp

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
