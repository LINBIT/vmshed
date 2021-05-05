package cmd

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"time"

	"github.com/apparentlymart/go-cidr/cidr"
	log "github.com/sirupsen/logrus"
)

func addNetwork(ctx context.Context, networkName string, network virterNet, ipNet *net.IPNet, dhcpID int, dhcpCount int) error {
	logger := log.WithFields(log.Fields{
		"Action":      "AddNetwork",
		"NetworkName": networkName,
	})

	argv := []string{"virter", "network", "add", networkName}
	if network.DHCP {
		if ipNet == nil {
			panic("cannot add network with DHCP without an IPNet")
		}
		gatewayAddress := cidr.Inc(ipNet.IP)
		networkCidr := net.IPNet{IP: gatewayAddress, Mask: ipNet.Mask}
		argv = append(argv, "--network-cidr", networkCidr.String(), "--dhcp")
	}
	if network.ForwardMode != "" {
		argv = append(argv, "--forward-mode", network.ForwardMode)
	}
	if network.Domain != "" {
		argv = append(argv, "--domain", network.Domain)
	}
	if dhcpCount > 0 {
		argv = append(argv, "--dhcp-id", strconv.Itoa(dhcpID), "--dhcp-count", strconv.Itoa(dhcpCount))
	}

	log.Printf("EXECUTING: %s", argv)
	err := cmdStderrTerm(ctx, logger, exec.Command(argv[0], argv[1:]...))
	if err != nil {
		log.WithError(err).Warnf("failed to create test network %s", networkName)
		return err
	}

	return nil
}

func removeNetwork(networkName string) error {
	logger := log.WithFields(log.Fields{
		"Action":      "RemoveNetwork",
		"NetworkName": networkName,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	argv := []string{"virter", "network", "rm", networkName}
	log.Printf("EXECUTING: %s", argv)
	err := cmdStderrTerm(ctx, logger, exec.Command(argv[0], argv[1:]...))
	if err != nil {
		logger.WithError(err).Warnf("failed to remove test network %s", networkName)
		return err
	}
	return nil
}

func accessNetwork() virterNet {
	return virterNet{
		Domain:      "test",
		ForwardMode: "nat",
		DHCP:        true,
	}
}
