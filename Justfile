PROJECT := "imapsync-go"
PKG := "github.com/greeddj/{{PROJECT}}"

VERSION := `git describe --tags --abbrev=0 2>/dev/null || echo "dev"`
COMMIT := `git rev-parse --short HEAD`
DATE := `date -u +%Y-%m-%dT%H:%M:%SZ`

FLAGS := "-s -w -extldflags '-static' -X {{PKG}}/cmd.Version={{VERSION}} -X {{PKG}}/cmd.Commit={{COMMIT}} -X {{PKG}}/cmd.Date={{DATE}} -X {{PKG}}/cmd.BuiltBy=just"
BUILD := "CGO_ENABLED=0 go build -mod vendor"

tools:
	@echo "===== Add tools ====="
	brew install golangci/tap/golangci-lint
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

deps:
	@echo "===== Check deps for {{PROJECT}} ====="
	go mod tidy
	go mod vendor

lint:
	@echo "===== Lint {{PROJECT}} ====="
	golangci-lint run ./...

check: deps
	@echo "===== Check {{PROJECT}} ====="
	go vet -mod vendor ./...
	staticcheck ./...
	govulncheck ./...

# Run unit tests
test:
	@echo "===== Unit tests for {{PROJECT}} ====="
	go test ./internal/...

build: check
	@echo "===== Build {{PROJECT}} ====="
	mkdir -p dist
	if [ -f dist/{{PROJECT}} ]; then rm -f dist/{{PROJECT}}; else echo "Not exist dist/{{PROJECT}}"; fi
	{{BUILD}} -ldflags="{{FLAGS}}" -o dist/{{PROJECT}} main.go

build_linux: check
	@echo "===== Build {{PROJECT}} for Linux / amd64 ====="
	mkdir -p dist
	if [ -f dist/{{PROJECT}} ]; then rm -f dist/{{PROJECT}}; else echo "Not exist dist/{{PROJECT}}"; fi
	GOOS="linux" GOARCH="amd64" {{BUILD}} -ldflags="{{FLAGS}}" -o dist/{{PROJECT}} main.go

oci executor="podman" tag="local": build_linux
	@echo "===== Build Local OCI {{PROJECT}} ====="
	cp dist/{{PROJECT}} imapsync-go
	{{executor}} build -t {{PROJECT}}:{{tag}} -f Dockerfile .
	rm -f imapsync-go
