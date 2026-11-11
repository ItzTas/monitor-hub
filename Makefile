MODULES := . sdk

.PHONY: check lint test generate

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
		(cd $$m && go test -race ./...) || exit 1; \
	done

generate:
	go tool buf generate
	go tool sqlc generate
