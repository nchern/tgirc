OUT=./bin/tgirc
GO_BUILD_FLAGS ?= -tags libtdjson
TDLIB_RPATH_LDFLAGS ?= -Wl,-rpath,/usr/local/lib
override export CGO_LDFLAGS := $(strip $(CGO_LDFLAGS) $(TDLIB_RPATH_LDFLAGS))


.PHONY: clean
clean:
	@find ./artifacts ./bin -type d -print0 | xargs -0 rm -rf


.PHONY: install-deps
install-deps:
	@go mod download


.PHONY: check
check:
	@staticcheck ./...


.PHONY: vet
vet:
	@go vet ./...


.PHONY: build
build: vet
	@go build $(GO_BUILD_FLAGS) -o $(OUT) .


.PHONY: install
install: build test
	@go install $(GO_BUILD_FLAGS) ./...


.PHONY: test
test: build
	go test $(GO_BUILD_FLAGS) -race -timeout=10s ./...


.PHONY: coverage
coverage: vet
	@./tools/coverage.sh


.PHONY: coverage-html
coverage-html: vet
	@./tools/coverage.sh html


.PHONY: local-run
local-run:
	while sleep 0.1; do find . -name "*.go" | entr -d -r go run $(GO_BUILD_FLAGS) main.go 2>&1 | cut -d ' ' -f1-2,4- ; done
