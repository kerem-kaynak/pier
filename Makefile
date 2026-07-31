# pier is a single binary that embeds the in-VM supervisor (both linux
# arches), so build the supervisors first.
ASSETS := cmd/pier/assets

build: supervisors
	go build -o pier ./cmd/pier

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
