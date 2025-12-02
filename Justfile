PROJECT := "imapsync-go"
PKG := "github.com/greeddj/{{PROJECT}}"

GIT_TAG := `git describe --tags --abbrev=0 2>/dev/null || git rev-parse --abbrev-ref HEAD`
GIT_COMMIT := `git rev-parse --short HEAD`

FLAGS := "-s -w -extldflags '-static' -X {{PKG}}/cmd.gitRef={{GIT_TAG}} -X {{PKG}}/cmd.gitCommit={{GIT_COMMIT}} -X {{PKG}}/cmd.appName={{PROJECT}}"
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
	if [ -f dist/{{PROJECT}}.bin ]; then rm -f dist/{{PROJECT}}.bin; else echo "Not exist dist/{{PROJECT}}.bin"; fi
	{{BUILD}} -ldflags="{{FLAGS}}" -o dist/{{PROJECT}}.bin main.go

build_linux: check
	@echo "===== Build {{PROJECT}} for Linux / amd64 ====="
	mkdir -p dist
	if [ -f dist/{{PROJECT}}.linux.amd64.bin ]; then rm -f dist/{{PROJECT}}.linux.amd64.bin; else echo "Not exist dist/{{PROJECT}}.linux.amd64.bin"; fi
	GOOS="linux" GOARCH="amd64" {{BUILD}} -ldflags="{{FLAGS}}" -o dist/{{PROJECT}}.linux.amd64.bin main.go

oci executor="podman" tag="local": build
	@echo "===== Build Local OCI {{PROJECT}} ====="
	{{executor}} build -t {{PROJECT}}:{{tag}} -f Dockerfile .
