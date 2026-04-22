.PHONY: rebuild run test check clean

all:
	cargo build

run:
	RUST_LOG=info,argus=debug,feishu=debug cargo run -- --config ./workspace/config.toml

test:
	cargo test --workspace

check:
	cargo fmt --all -- --check
	cargo clippy --workspace -- -D warnings
	cargo test --workspace

clean:
	cargo clean

rebuild: clean all
