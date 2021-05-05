package cmd_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/LINBIT/vmshed/cmd"
)

func TestNewNetworkList(t *testing.T) {
	_, workingBase, _ := net.ParseCIDR("10.224.0.0/24")
	list := cmd.NewNetworkList(workingBase)
	first := list.ReserveNext()
	second := list.ReserveNext()
	third := list.ReserveNext()
	assert.Equal(t, "10.224.0.0/24", first.String())
	assert.Equal(t, "10.224.1.0/24", second.String())
	assert.Equal(t, "10.224.2.0/24", third.String())
	list.Free(second)
	assert.Equal(t, "10.224.1.0/24", list.ReserveNext().String())
	assert.Equal(t, "10.224.3.0/24", list.ReserveNext().String())

}
