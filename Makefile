.PHONY: all build test vet fmt clean spikes

all: test

build:
	mkdir -p bin
	go build -o bin/paas ./cmd/paas
	go build -o bin/paasd ./cmd/paasd
	go build -o bin/router-spike ./cmd/router-spike
	go build -o bin/hello-go ./examples/hello-go
	go build -o bin/unhealthy ./examples/unhealthy
	go build -o bin/memory-hog ./examples/memory-hog

vet:
	go vet ./...

fmt:
	gofmt -w $$(rg --files -g '*.go')

spikes:
	./scripts/spikes/run-all.sh

test: vet build
	go test ./...
	./scripts/tests/cli-ping.sh
	./scripts/tests/broken-build.sh
	./scripts/tests/examples.sh
	./scripts/spikes/router.sh
	./scripts/spikes/run-all.sh

clean:
	rm -rf bin
