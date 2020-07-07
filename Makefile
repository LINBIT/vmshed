all: vmshed

SOURCES=$(wildcard cmd/*.go) cmd/config/config.go
VERSION=`git describe --tags --always --dirty`
LDFLAGS=-ldflags "-X github.com/LINBIT/vmshed/cmd/config.Version=$(VERSION)"

.PHONY: vmshed
vmshed: $(SOURCES)
	NAME="$@"; [ -n "$(GOOS)" ] && NAME="$${NAME}-$(GOOS)"; \
	[ -n "$(GOARCH)" ] && NAME="$${NAME}-$(GOARCH)"; \
	go build -o "$$NAME" $(LDFLAGS) .

.PHONY: release
release:
	make vmshed GOOS=linux GOARCH=amd64
