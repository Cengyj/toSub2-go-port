.PHONY: build frontend test race vet run

build:
	./scripts/build.sh

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
