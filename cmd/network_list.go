package cmd

import (
	"net"

	"github.com/apparentlymart/go-cidr/cidr"
)

type networkList struct {
	currentV4 *net.IPNet
	currentV6 *net.IPNet
	freeNets  map[string]bool
}

func NewNetworkList(currentV4, currentV6 *net.IPNet) *networkList {
	return &networkList{
		currentV4: currentV4,
		currentV6: currentV6,
		freeNets:  make(map[string]bool),
	}
}

func (n *networkList) ReserveNext(ipv6 bool) *net.IPNet {
	for k, v := range n.freeNets {
		ipNet := mustParse(k)
		isIPv6 := ipNet.IP.To4() == nil

		if v && isIPv6 == ipv6 {
			n.freeNets[k] = false
			return ipNet
		}
	}

	candidate := n.currentV4
	if ipv6 {
		candidate = n.currentV6
	}

	prefix, _ := candidate.Mask.Size()
	next, exceed := cidr.NextSubnet(candidate, prefix)
	if exceed {
		panic("available subnets exhausted")
	}

	n.freeNets[candidate.String()] = false
	if ipv6 {
		n.currentV6 = next
	} else {
		n.currentV4 = next
	}

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
