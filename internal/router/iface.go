package router

import (
	"fmt"
	"net"
)

// GetCIDRFromInterfaceName returns the first IPv4 address and subnet of an interface.
func GetCIDRFromInterfaceName(name string) (*net.IP, *net.IPNet, error) {
	iface, e := net.InterfaceByName(name)
	if e != nil {
		return nil, nil, e
	}
	addresses, e := iface.Addrs()
	if e != nil {
		return nil, nil, e
	}
	if len(addresses) == 0 {
		return nil, nil, fmt.Errorf("%s has no address", name)
	}
	for _, address := range addresses {
		ip, ipnet, e := net.ParseCIDR(address.String())
		if e == nil && ip.To4() != nil {
			return &ip, ipnet, nil
		}
	}
	return nil, nil, fmt.Errorf("%s has no IPv4 address", name)
}
