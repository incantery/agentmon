build:
	go build -o bin/agentmon ./cmd/agentmon

test:
	go test ./...

.PHONY: build test
