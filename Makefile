OUT=tgirc


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


.PHONY: local-run
local-run:
	@go run main.go
