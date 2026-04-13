.PHONY: build run run-cli test clean

build:
	go build -o bin/argus ./cmd/argus

run: build
	./bin/argus --mode server --config config.yaml

run-cli: build
	./bin/argus --mode cli --config config.yaml

test:
	go test ./...

clean:
	rm -rf bin/
