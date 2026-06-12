.PHONY: test tidy api worker

tidy:
	go mod tidy

test:
	go test ./...

api:
	go run ./cmd/api

worker:
	go run ./cmd/worker
