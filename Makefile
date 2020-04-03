all: vmshed

SOURCES=$(wildcard cmd/*.go) cmd/config/config.go
VERSION=`git describe --tags --always --dirty`
LDFLAGS=-ldflags "-X github.com/LINBIT/vmshed/cmd/config.Version=$(VERSION)"

vmshed: $(SOURCES)
	go build $(LDFLAGS) .
