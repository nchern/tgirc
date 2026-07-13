NAME=tgterm
TAG=latest
BASE_BUILDER_IMAGE_NAME=base-builder:latest
IMAGE_NAME=$(NAME):$(TAG)
OUT=$(NAME)

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
	@go build -o bin/$(OUT) .


.PHONY: install
install: build test
	@go install ./...


.PHONY: test
test: build
	go test -race -timeout=10s ./...


.PHONY: coverage
coverage: vet
	@./tools/coverage.sh


.PHONY: coverage-html
coverage-html: vet
	@./tools/coverage.sh html


# .PHONY: local-run
# local-run:
# 	@cp artifacts/errors.log artifacts/errors.log.1 2> /dev/null || true
# 	@go run main.go 2> artifacts/errors.log || tail -n20 artifacts/errors.log
