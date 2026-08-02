# pier is a single binary that embeds the in-VM supervisor (both linux
# arches), so build the supervisors first.
ASSETS := cmd/pier/assets

# git describe in a checkout; release tarballs have no .git, so the brew
# formula passes VERSION explicitly.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build: supervisors
	go build -ldflags "-X main.version=$(VERSION)" -o pier ./cmd/pier

supervisors:
	GOOS=linux GOARCH=arm64 go build -o $(ASSETS)/pier-supervisor-linux-arm64 ./cmd/pier-supervisor
	GOOS=linux GOARCH=amd64 go build -o $(ASSETS)/pier-supervisor-linux-amd64 ./cmd/pier-supervisor

# install(1), not cp: it unlinks the target first (new inode), so replacing
# the binary is safe even while a pier process is running (attached ssh).
install: build
	install -m 0755 pier $$(go env GOPATH)/bin/pier

test:
	go vet ./... && go test ./...

clean:
	rm -f pier $(ASSETS)/pier-supervisor-linux-*
