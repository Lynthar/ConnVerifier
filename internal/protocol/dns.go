package protocol

import (
	"encoding/binary"
	"errors"
)

// The one DNS exchange the dns check makes (RFC 1035 §4.1): a recursive query for
// the root zone's NS records with an EDNS0 OPT record (RFC 6891), and the header and
// question of its answer. Nothing else of DNS is spoken here.
const (
	dnsHeaderLen = 12
	dnsTypeNS    = 2
	dnsTypeOPT   = 41
	dnsClassIN   = 1
	dnsFlagQR    = 0x8000
	dnsFlagRD    = 0x0100
	// DNSPayload is the EDNS0 UDP payload size offered: 1232 bytes fits the IPv6
	// minimum MTU, so an answer is not fragmented.
	DNSPayload = 1232
)

// RootNSQuery returns the query with the given ID.
func RootNSQuery(id uint16) []byte {
	b := make([]byte, 0, dnsHeaderLen+5+11)
	b = binary.BigEndian.AppendUint16(b, id)
	b = binary.BigEndian.AppendUint16(b, dnsFlagRD)
	b = append(b, 0, 1, 0, 0, 0, 0, 0, 1) // one question, one additional record
	b = append(b, 0)                      // the root name
	b = binary.BigEndian.AppendUint16(b, dnsTypeNS)
	b = binary.BigEndian.AppendUint16(b, dnsClassIN)
	b = append(b, 0) // OPT: root owner, type, payload size in the class field
	b = binary.BigEndian.AppendUint16(b, dnsTypeOPT)
	b = binary.BigEndian.AppendUint16(b, DNSPayload)
	return append(b, 0, 0, 0, 0, 0, 0) // extended code, version 0, no flags, no options
}

var errNotRootNSAnswer = errors.New("not an answer to a root NS query")

// ParseRootNSAnswer reads an answer to RootNSQuery: its ID and response code. It
// rejects a packet that is not a response to that one question, so a stray packet
// cannot pass for an answer; a refusal may leave the question out.
func ParseRootNSAnswer(b []byte) (id uint16, rcode int, err error) {
	if len(b) < dnsHeaderLen {
		return 0, 0, errNotRootNSAnswer
	}
	id, flags, questions := binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:]), binary.BigEndian.Uint16(b[4:])
	rcode = int(flags & 0xf)
	switch {
	case flags&dnsFlagQR == 0 || flags>>11&0xf != 0: // a query, or not a standard one
		return 0, 0, errNotRootNSAnswer
	case questions == 0 && rcode != 0:
		return id, rcode, nil
	case questions != 1 || len(b) < dnsHeaderLen+5 || b[dnsHeaderLen] != 0 ||
		binary.BigEndian.Uint16(b[dnsHeaderLen+1:]) != dnsTypeNS || binary.BigEndian.Uint16(b[dnsHeaderLen+3:]) != dnsClassIN:
		return 0, 0, errNotRootNSAnswer
	}
	return id, rcode, nil
}
