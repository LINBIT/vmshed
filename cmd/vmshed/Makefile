all: vmshed

SOURCES=$(wildcard *.go) config/config.go
VERSION=`git describe --tags --always --dirty`
LDFLAGS=-ldflags "-X github.com/LINBIT/lbtest/cmd/vmshed/config.Version=$(VERSION)"

vmshed: $(SOURCES)
	go build $(LDFLAGS)
