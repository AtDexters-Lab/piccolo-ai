.PHONY: build check test test-race vet image

build:
	mkdir -p bin
	go build -o bin/piccolo-ai-ovms ./cmd/piccolo-ai-ovms

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

check: test test-race vet

image:
	docker build -f backends/ovms/Dockerfile -t piccolo-ai-ovms:dev .
