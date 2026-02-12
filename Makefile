SHELL := /usr/bin/env bash

.PHONY: build test tcp-demo lint

build:
	go build ./cmd/simchain

test:
	go test ./...

tcp-demo:
	./scripts/run-tcp-demo.sh
