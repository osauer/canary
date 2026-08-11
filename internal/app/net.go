package app

import (
	"net"
	"strings"
)

type interfaceAddrsFunc func() ([]net.Addr, error)

// PublicURLForAddr returns the browser-visible URL for an app listen address.
// A wildcard host is replaced with the first usable LAN IPv4 address, falling
// back to 127.0.0.1 when none is available. An empty addr uses [DefaultAddr].
// The result uses HTTP and preserves the listen port.
func PublicURLForAddr(addr string) string {
	return publicURLForAddr(addr, net.InterfaceAddrs)
}

func publicURLForAddr(addr string, addrs interfaceAddrsFunc) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if isWildcardHost(host) {
		if lan := firstLANIPv4(addrs); lan != "" {
			host = lan
		} else {
			host = "127.0.0.1"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

// LoopbackAddrForLocalConnect returns a host:port address suitable for a local
// client connecting to the app server. Wildcard hosts are replaced with
// 127.0.0.1, an empty value uses [DefaultAddr], and non-host:port input is
// returned unchanged.
func LoopbackAddrForLocalConnect(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if isWildcardHost(host) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func isWildcardHost(host string) bool {
	return host == "" || host == "0.0.0.0" || host == "::"
}

// loopbackBindAddr reports whether addr binds only the loopback interface.
func loopbackBindAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstLANIPv4(addrs interfaceAddrsFunc) string {
	if addrs == nil {
		return ""
	}
	items, err := addrs()
	if err != nil {
		return ""
	}
	for _, item := range items {
		ip, ok := addrIP(item)
		if !ok || ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func addrIP(addr net.Addr) (net.IP, bool) {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP, true
	case *net.IPAddr:
		return v.IP, true
	default:
		return nil, false
	}
}
