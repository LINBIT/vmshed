package cmd_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/LINBIT/vmshed/cmd"
)

func TestNewNetworkList(t *testing.T) {
	_, workingBase, _ := net.ParseCIDR("10.224.0.0/24")
	_, ipv6Base, _ := net.ParseCIDR("fd62:a80c:412::/64")
	list := cmd.NewNetworkList(workingBase, ipv6Base)
	first := list.ReserveNext(false)
	second := list.ReserveNext(false)
	third := list.ReserveNext(false)
	forth := list.ReserveNext(true)
	fifth := list.ReserveNext(true)
	assert.Equal(t, "10.224.0.0/24", first.String())
	assert.Equal(t, "10.224.1.0/24", second.String())
	assert.Equal(t, "10.224.2.0/24", third.String())
	assert.Equal(t, "fd62:a80c:412::/64", forth.String())
	assert.Equal(t, "fd62:a80c:412:1::/64", fifth.String())
	list.Free(second)
	list.Free(forth)
	assert.Equal(t, "10.224.1.0/24", list.ReserveNext(false).String())
	assert.Equal(t, "10.224.3.0/24", list.ReserveNext(false).String())
	assert.Equal(t, "fd62:a80c:412::/64", list.ReserveNext(true).String())
}
