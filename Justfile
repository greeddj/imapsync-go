PROJECT := "imapsync-go"
# Skip tags whose ref name holds a character outside this set. An annotated tag can carry another name, so an unsafe result falls back to the commit hash.
VERSION := `sh -c 'export LC_ALL=C; v=$(git describe --tags --always --dirty --exclude="*[!A-Za-z0-9._+/-]*" 2>/dev/null) || v=dev; case $v in *[!A-Za-z0-9._+/-]*) v=$(git describe --always --dirty --exclude="*" 2>/dev/null) || v=dev ;; esac; echo "$v"'`
COMMIT := `sh -c 'git rev-parse --short HEAD 2>/dev/null || true'`
DATE := `date -u +%Y-%m-%dT%H:%M:%SZ`
LDFLAGS := "-s -w" \
  + " -X main.Version=" + VERSION \
  + " -X main.Commit=" + COMMIT \
  + " -X main.Date=" + DATE \
  + " -X main.BuiltBy=just"

deps:
	@echo "===== Check deps for {{PROJECT}} ====="
	go mod tidy
	go mod vendor

lint:
	@echo "===== Lint {{PROJECT}} ====="
	golangci-lint run ./... --timeout=5m

fix:
	@echo "===== Fix {{PROJECT}} ====="
	go fmt ./...
	go fix ./...
	go tool fieldalignment -fix ./...

test:
	@echo "===== Test {{PROJECT}} ====="
	go test ./...

check:
	@echo "===== Check {{PROJECT}} ====="
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...
	go tool fieldalignment ./...

run: check lint test
	@echo "===== Run {{PROJECT}} ====="
	go run -race ./cmd/{{ PROJECT }}/main.go

build: check lint test
	@echo "===== Build {{PROJECT}} ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	CGO_ENABLED=0 go build -trimpath -ldflags={{quote(LDFLAGS)}}  -o ./dist/{{PROJECT}} ./cmd/{{ PROJECT }}/main.go

build_linux: check
	@echo "===== Build {{PROJECT}} for Linux / amd64 ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	GOOS="linux" GOARCH="amd64" CGO_ENABLED=0 go build -trimpath -ldflags={{quote(LDFLAGS)}} -o dist/{{PROJECT}} ./cmd/{{ PROJECT }}/main.go

oci executor="podman" tag="local": build_linux
	@echo "===== Build Local OCI {{PROJECT}} ====="
	rm -rf dist/oci
	mkdir -p dist/oci/linux/amd64/dist
	cp dist/{{PROJECT}} dist/oci/linux/amd64/dist/{{PROJECT}}
	{{executor}} build --platform=linux/amd64 -t {{PROJECT}}:{{tag}} -f Dockerfile dist/oci
