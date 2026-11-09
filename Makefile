.PHONY: fmt test vet build run docker-build

fmt:
	go fmt ./...

test:
	go test ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o recharge ./cmd/server

run:
	go run ./cmd/server

docker-build:
	docker compose build
