.PHONY: all build test vet fmt clean spikes integration test-privileged

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
	find . -name '*.go' -print0 | xargs -0 gofmt -w

spikes:
	./scripts/spikes/run-all.sh

integration: build
	go test -count=1 -tags integration -v ./tests

test-privileged: build
	go test -c -tags integration -o bin/acceptance.test ./tests
	go test -c -o bin/builder.test ./internal/builder
	sudo ./bin/builder.test -test.run TestRootBuildResourceProperties -test.v
	cd tests && sudo ../bin/acceptance.test -test.count=1 -test.v

test: vet build
	go test ./...
	./scripts/tests/cli-ping.sh
	./scripts/tests/broken-build.sh
	./scripts/tests/examples.sh
	./scripts/spikes/router.sh
	./scripts/spikes/run-all.sh
	go test -count=1 -tags integration -v ./tests

clean:
	rm -rf bin
