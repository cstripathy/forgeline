.PHONY: build test vet ci clean run-local

build:
	go build -o bin/forgeline ./cmd/forgeline

test:
	go test ./...

vet:
	go vet ./...

# Everything the CI workflow runs, runnable locally.
ci: vet test build

clean:
	rm -rf bin

# Start the server on this machine only, with a generated token and in-memory history.
# Needs Docker for steps to run. Open the link it prints.
run-local: build
	./bin/forgeline server --data-dir .forgeline-server
