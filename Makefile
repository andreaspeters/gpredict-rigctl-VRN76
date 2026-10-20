#Dockerfile vars
#vars
BUILDDATE=$(shell date -u +%Y%m%d)


.PHONY: help build all test go-fmt update-gomod

.DEFAULT_GOAL := all

build:
	@echo ">>>> Build binary"
	go build .


update-gomod:
	go get -u
	go mod tidy

go-fmt:
	@gofmt -w .

test:
	@echo ">>> Run tests"
	go test -count=1 ./...

all: go-fmt build
