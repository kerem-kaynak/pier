# pier is a single binary that embeds the in-VM supervisor (both linux
# arches), so build the supervisors first.
ASSETS := cmd/pier/assets

build: supervisors
	go build -o pier ./cmd/pier

supervisors:
	GOOS=linux GOARCH=arm64 go build -o $(ASSETS)/pier-supervisor-linux-arm64 ./cmd/pier-supervisor
	GOOS=linux GOARCH=amd64 go build -o $(ASSETS)/pier-supervisor-linux-amd64 ./cmd/pier-supervisor

install: build
	cp pier $$(go env GOPATH)/bin/pier

test:
	go vet ./... && go test ./...

clean:
	rm -f pier $(ASSETS)/pier-supervisor-linux-*
