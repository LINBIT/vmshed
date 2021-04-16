package cmd

import (
	"net"

	"github.com/apparentlymart/go-cidr/cidr"
)

type networkList struct {
	current  *net.IPNet
	freeNets map[string]bool
}

func NewNetworkList(current *net.IPNet) *networkList {
	return &networkList{
		current:  current,
		freeNets: make(map[string]bool),
	}
}

func (n *networkList) ReserveNext() *net.IPNet {
	for k, v := range n.freeNets {
		if v {
			n.freeNets[k] = false
			return mustParse(k)
		}
	}

	candidate := n.current

	next, exceed := cidr.NextSubnet(n.current, len(n.current.IP)*8-8)
	if exceed {
		panic("available subnets exhausted")
	}

	n.freeNets[n.current.String()] = false
	n.current = next
	return candidate
}

func (n *networkList) Free(ipNet *net.IPNet) {
	if ipNet == nil {
		return
	}

	n.freeNets[ipNet.String()] = true
}

func mustParse(cidr string) *net.IPNet {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}

	return ipnet
}
