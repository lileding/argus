.PHONY: build run run-cli test clean sandbox

build:
	go build -o bin/argus ./cmd/argus

run: build
	./bin/argus --mode server --workspace ./workspace

run-cli: build
	./bin/argus --mode cli --workspace ./workspace

test:
	go test ./...

sandbox:
	docker build -f Dockerfile.sandbox -t argus-sandbox:latest .

clean:
	rm -rf bin/
