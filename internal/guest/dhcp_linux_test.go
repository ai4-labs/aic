//go:build linux

package guest

import (
	"bytes"
	"net"
	"testing"
)

func TestDHCPPackets(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x61, 0x69, 0x63, 0x00, 0x03}
	msg := dhcpMessage(dhcpRequest, [4]byte{1, 2, 3, 4}, mac, map[byte][]byte{optRequested: {10, 0, 0, 5}})
	pkt := udpPacket(msg)
	if checksum(pkt[:20]) != 0 {
		t.Error("bad IPv4 header checksum")
	}
	// Turn it into a reply to port 68 and parse it back.
	pkt[22], pkt[23] = 0, 68
	got := udpPayload(pkt, 68)
	if !bytes.Equal(got, msg) {
		t.Fatalf("payload = %d bytes, want %d", len(got), len(msg))
	}
	opts := parseOptions(got[240:])
	if !bytes.Equal(opts[optMsgType], []byte{dhcpRequest}) || !bytes.Equal(opts[optRequested], []byte{10, 0, 0, 5}) {
		t.Errorf("options = %v", opts)
	}
	if udpPayload(pkt, 67) != nil {
		t.Error("payload for the wrong port")
	}

	l, err := staticLease(mac)
	if err != nil || !l.ip.Equal(net.IPv4(192, 168, 64, 251)) || !l.router.Equal(net.IPv4(192, 168, 64, 1)) {
		t.Errorf("staticLease = %+v, %v", l, err)
	}
	if _, err := staticLease(net.HardwareAddr{0x0a, 0, 0, 0, 0, 1}); err == nil {
		t.Error("static address for a foreign MAC")
	}
}
