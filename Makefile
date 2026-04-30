.PHONY: rebuild image test check clean

all:
	cargo build

image:
	docker build -t argus:latest .

test:
	cargo test --workspace

check:
	cargo fmt --all -- --check
	cargo clippy --workspace -- -D warnings
	cargo test --workspace

clean:
	cargo clean

rebuild: clean all
