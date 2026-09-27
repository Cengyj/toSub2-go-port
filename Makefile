.PHONY: build frontend test race vet run

build:
	go build -o bin/tosub2 ./cmd/tosub2

frontend:
	./scripts/build-frontend.sh

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/tosub2
