package netenv

import (
	"errors"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

func kindOf(ifi net.Interface) string {
	aa, err := adapters()
	if err != nil {
		return KindUnknown
	}
	for a := aa; a != nil; a = a.Next {
		index := a.IfIndex // the index package net gives the interface
		if index == 0 {
			index = a.Ipv6IfIndex
		}
		if int(index) == ifi.Index {
			return kindOfIfType(a.IfType)
		}
	}
	return KindUnknown
}

// ifTypePropVirtual is IF_TYPE_PROP_VIRTUAL, which Wintun adapters report:
// WireGuard and the TUN modes of proxy clients.
const ifTypePropVirtual = 53

func kindOfIfType(t uint32) string {
	switch t {
	case windows.IF_TYPE_IEEE80211:
		return KindWiFi
	case windows.IF_TYPE_ETHERNET_CSMACD:
		return KindWired
	case windows.IF_TYPE_PPP:
		return KindPPP
	case windows.IF_TYPE_TUNNEL, ifTypePropVirtual:
		return KindTunnel
	}
	return KindOther
}

// adapters lists the adapters with only the fields kindOf reads, growing the
// buffer for as long as Windows asks for a larger one.
func adapters() (*windows.IpAdapterAddresses, error) {
	const skip = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_DNS_SERVER | windows.GAA_FLAG_SKIP_FRIENDLY_NAME
	size := uint32(15000)
	for {
		b := make([]byte, size)
		aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&b[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, skip, 0, aa, &size)
		switch {
		case err == nil && size == 0:
			return nil, nil
		case err == nil:
			return aa, nil
		case !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) || size <= uint32(len(b)):
			return nil, err
		}
	}
}
