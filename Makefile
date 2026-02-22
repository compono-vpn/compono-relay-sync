BINARY   := relay-sync
GOFLAGS  := -ldflags="-s -w"

.PHONY: build build-all clean

build:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY)-linux-amd64 .

build-all:
	mkdir -p bin
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY)-darwin-arm64 .

docker:
	docker build -t $(BINARY) .

clean:
	rm -rf bin
