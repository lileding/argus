.PHONY: build run run-cli test clean

build:
	go build -o bin/argus ./cmd/argus

run: build
	./bin/argus --mode server --workspace ./workspace

run-cli: build
	./bin/argus --mode cli --workspace ./workspace

test:
	go test ./...

clean:
	rm -rf bin/
