.PHONY: build test vet ci clean

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
