.PHONY: build run run-cli test clean sandbox up down ratex ratex-clean

# Path to the RaTeX static library produced by cargo.
RATEX_LIB := third_party/ratex/target/release/libratex_bridge.a

# Build the Rust static library (RaTeX LaTeX renderer, linked into the Go
# binary via CGo). Requires cargo + rustc on PATH. Output is consumed by
# internal/render/latex.go — see its #cgo LDFLAGS directive.
ratex: $(RATEX_LIB)

$(RATEX_LIB):
	cd third_party/ratex && cargo build --release

ratex-clean:
	cd third_party/ratex && cargo clean

# Main build depends on the RaTeX static library. First invocation pulls
# Rust crates from the network and may take several minutes; subsequent
# builds are incremental via cargo's cache.
build: $(RATEX_LIB)
	go build -o bin/argus ./cmd/argus

run: build
	./bin/argus --mode server --workspace ./workspace

run-cli: build
	./bin/argus --mode cli --workspace ./workspace

test: $(RATEX_LIB)
	go test ./...

sandbox:
	docker build -f Dockerfile.sandbox -t argus-sandbox:latest .

up:
	docker compose up -d postgres

down:
	docker compose down

# `clean` removes only the Go binary. Use `ratex-clean` separately to rebuild
# the Rust static library from scratch.
clean:
	rm -rf bin/
