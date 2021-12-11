.PHONY: pre-commit

pre-commit: *.go
	go build -o /dev/null .
	go vet ./...
	golangci-lint run ./...
	go test ./...
	go test ./... -race
	go test ./... -cover