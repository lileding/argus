.PHONY: build run test check clean

build:
	cargo build

run:
	RUST_LOG=info,argus=debug,feishu=debug FEISHU_APP_ID=cli_a957a04745f8dbcf FEISHU_APP_SECRET=rRuVxRDkXGEyGZ6UlLlPghVAaS7pZPrH cargo run

test:
	cargo test --workspace

check:
	cargo clippy --workspace -- -D warnings
	cargo test --workspace

clean:
	cargo clean
