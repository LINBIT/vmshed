all: vmshed

SOURCES=$(wildcard cmd/*.go) cmd/config/config.go
VERSION=`git describe --tags --always --dirty`
LDFLAGS=-ldflags "-X github.com/LINBIT/vmshed/cmd/config.Version=$(VERSION)"

.PHONY: vmshed
vmshed: $(SOURCES)
	NAME="$@"; [ -n "$(GOOS)" ] && NAME="$${NAME}-$(GOOS)"; \
	[ -n "$(GOARCH)" ] && NAME="$${NAME}-$(GOARCH)"; \
	go build -o "$$NAME" $(LDFLAGS) .

tests/mockvirter/mockvirter: tests/mockvirter/main.go
	go build -o $@ ./tests/mockvirter

.PHONY: integration-test
integration-test: vmshed tests/mockvirter/mockvirter
	VMSHED_BINARY=$(PWD)/vmshed MOCK_VIRTER_BINARY=$(PWD)/tests/mockvirter/mockvirter go test -v ./tests/

.PHONY: release
release:
	make vmshed GOOS=linux GOARCH=amd64
