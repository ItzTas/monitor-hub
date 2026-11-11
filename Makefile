MODULES := . sdk

.PHONY: check lint test generate generate-rust generate-python check-rust

check: lint test

lint:
	test -z "$$(gofmt -l .)"
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "lint $$m"; \
		(cd $$m && go vet ./... && golangci-lint run ./...) || exit 1; \
	done

test:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "test $$m"; \
		(cd $$m && go test -race -timeout 30m ./...) || exit 1; \
	done

generate:
	go tool buf generate
	go tool sqlc generate

generate-rust:
	cd sdk-rust && cargo run --quiet -p monitor-hub-codegen

check-rust:
	cd sdk-rust && cargo fmt --all --check
	cd sdk-rust && cargo clippy --workspace --all-targets --all-features -- -D warnings
	cd sdk-rust && cargo test --workspace --all-features

generate-python:
	uvx --from grpcio-tools==1.78.0 python -m grpc_tools.protoc -Iproto/monitorhub/plugin/v1 --python_out=examples/python-plugin/gen --pyi_out=examples/python-plugin/gen --grpc_python_out=examples/python-plugin/gen proto/monitorhub/plugin/v1/plugin.proto
