//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The VM's network is configured by the guest with DHCP, in the
// background: containers without a network don't wait for it, and a slow
// DHCP server on the host can't keep the VM from booting.

// network is the state of the VM's network configuration.
var network struct {
	once  sync.Once
	ready chan struct{} // closed when DHCP finished, successfully or not
	err   error
}

// startNetwork configures eth0 with DHCP in the background.
func startNetwork() {
	network.once.Do(func() {
		network.ready = make(chan struct{})
		go func() {
			defer close(network.ready)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			network.err = configureNetwork(ctx, "eth0")
			if network.err != nil {
				log.Printf("aic-guest: network: %v", network.err)
			}
		}()
	})
}

// waitNetwork waits up to timeout for the network to be configured.
func waitNetwork(timeout time.Duration) error {
	startNetwork()
	select {
	case <-network.ready:
		return network.err
	case <-time.After(timeout):
		return errors.New("DHCP has not answered yet")
	}
}

func configureNetwork(ctx context.Context, ifname string) error {
	if err := linkUp("lo"); err != nil {
		return err
	}
	if err := linkUp(ifname); err != nil {
		return err
	}
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	l, err := dhcp(dctx, ifi)
	cancel()
	if err != nil {
		// The host's DHCP server (bootpd) sometimes stops answering
		// while its NAT keeps working: fall back to a fixed address.
		log.Printf("aic-guest: DHCP on %s: %v; using a static address", ifname, err)
		if l, err = staticLease(ifi.HardwareAddr); err != nil {
			return err
		}
	}
	if err := addAddress(ifi.Index, l.ip, l.mask); err != nil {
		return fmt.Errorf("configuring %s: %w", l.ip, err)
	}
	if l.router != nil {
		if err := addDefaultRoute(ifi.Index, l.router); err != nil {
			return fmt.Errorf("adding a route via %s: %w", l.router, err)
		}
	}
	var resolv strings.Builder
	for _, d := range l.dns {
		fmt.Fprintf(&resolv, "nameserver %s\n", d)
	}
	return os.WriteFile("/etc/resolv.conf", []byte(resolv.String()), 0o644)
}

func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bringing up %s: %w", name, err)
	}
	return nil
}

// lease is what a DHCP server assigned.
type lease struct {
	ip, router net.IP
	mask       net.IPMask
	dns        []net.IP
}

// DHCP message types and options (RFC 2131, 2132).
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6

	optSubnet    = 1
	optRouter    = 3
	optDNS       = 6
	optRequested = 50
	optMsgType   = 53
	optServerID  = 54
	optParams    = 55
	optEnd       = 255
)

// dhcp obtains a lease on ifi, retrying until ctx is done. It uses a
// packet socket, since the interface has no address to send from yet.
func dhcp(ctx context.Context, ifi *net.Interface) (*lease, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifi.Index}); err != nil {
		return nil, err
	}
	var xid [4]byte
	rand.Read(xid[:])
	send := func(msg []byte) error {
		bcast := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifi.Index, Halen: 6}
		copy(bcast.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		return unix.Sendto(fd, udpPacket(msg), 0, bcast)
	}
	// recv waits for a reply of one of the given types, up to wait.
	recv := func(wait time.Duration, types ...byte) (byte, []byte, map[byte][]byte, error) {
		deadline := time.Now().Add(wait)
		buf := make([]byte, 1500)
		for {
			left := time.Until(deadline)
			if left <= 0 || ctx.Err() != nil {
				return 0, nil, nil, errors.New("no reply")
			}
			tv := unix.NsecToTimeval(min(left, 500*time.Millisecond).Nanoseconds())
			unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				continue // timeout or EINTR
			}
			msg := udpPayload(buf[:n], 68)
			if len(msg) < 240 || msg[0] != 2 || !bytes.Equal(msg[4:8], xid[:]) ||
				!bytes.Equal(msg[28:34], ifi.HardwareAddr) {
				continue
			}
			opts := parseOptions(msg[240:])
			if t := opts[optMsgType]; len(t) == 1 && bytes.IndexByte(types, t[0]) >= 0 {
				return t[0], msg, opts, nil
			}
		}
	}

	for attempt := 0; ctx.Err() == nil; attempt++ {
		wait := min(time.Duration(1<<min(attempt, 3))*time.Second, 8*time.Second)
		if err := send(dhcpMessage(dhcpDiscover, xid, ifi.HardwareAddr, nil)); err != nil {
			return nil, err
		}
		_, offer, opts, err := recv(wait, dhcpOffer)
		if err != nil {
			continue
		}
		yiaddr := net.IP(append([]byte(nil), offer[16:20]...))
		extra := map[byte][]byte{optRequested: yiaddr.To4(), optServerID: opts[optServerID]}
		if err := send(dhcpMessage(dhcpRequest, xid, ifi.HardwareAddr, extra)); err != nil {
			return nil, err
		}
		typ, ack, opts, err := recv(wait, dhcpAck, dhcpNak)
		if err != nil || typ == dhcpNak {
			continue
		}
		l := &lease{ip: net.IP(append([]byte(nil), ack[16:20]...)), mask: net.CIDRMask(24, 32)}
		if m := opts[optSubnet]; len(m) == 4 {
			l.mask = net.IPMask(m)
		}
		if r := opts[optRouter]; len(r) >= 4 {
			l.router = net.IP(append([]byte(nil), r[:4]...))
		}
		for d := opts[optDNS]; len(d) >= 4; d = d[4:] {
			l.dns = append(l.dns, net.IP(append([]byte(nil), d[:4]...)))
		}
		return l, nil
	}
	return nil, errors.New("no answer from a DHCP server")
}

func dhcpMessage(typ byte, xid [4]byte, mac net.HardwareAddr, extra map[byte][]byte) []byte {
	m := make([]byte, 240, 300)
	m[0], m[1], m[2] = 1, 1, 6 // BOOTREQUEST, Ethernet, address length
	copy(m[4:8], xid[:])
	binary.BigEndian.PutUint16(m[10:12], 0x8000) // replies by broadcast: we have no address yet
	copy(m[28:34], mac)
	copy(m[236:240], []byte{99, 130, 83, 99}) // magic cookie
	m = append(m, optMsgType, 1, typ)
	for _, code := range []byte{optRequested, optServerID} {
		if v := extra[code]; len(v) > 0 {
			m = append(m, code, byte(len(v)))
			m = append(m, v...)
		}
	}
	m = append(m, optParams, 3, optSubnet, optRouter, optDNS, optEnd)
	return m
}

func parseOptions(b []byte) map[byte][]byte {
	opts := map[byte][]byte{}
	for len(b) > 0 {
		code := b[0]
		if code == optEnd {
			break
		}
		if code == 0 { // pad
			b = b[1:]
			continue
		}
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			break
		}
		opts[code] = b[2 : 2+int(b[1])]
		b = b[2+int(b[1]):]
	}
	return opts
}

// udpPacket wraps a DHCP message in UDP and IPv4 headers, from 0.0.0.0:68
// to 255.255.255.255:67.
func udpPacket(payload []byte) []byte {
	p := make([]byte, 28+len(payload))
	ip := p[:20]
	ip[0] = 0x45 // IPv4, 20-byte header
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(p)))
	ip[8] = 64 // TTL
	ip[9] = unix.IPPROTO_UDP
	copy(ip[16:20], []byte{255, 255, 255, 255})
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	udp := p[20:28]
	binary.BigEndian.PutUint16(udp[0:2], 68)
	binary.BigEndian.PutUint16(udp[2:4], 67)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(payload))) // no checksum: optional in IPv4
	copy(p[28:], payload)
	return p
}

// udpPayload returns the payload of an IPv4 UDP packet to port, or nil.
func udpPayload(p []byte, port uint16) []byte {
	if len(p) < 20 || p[0]>>4 != 4 || p[9] != unix.IPPROTO_UDP {
		return nil
	}
	ihl := int(p[0]&0x0f) * 4
	if len(p) < ihl+8 || binary.BigEndian.Uint16(p[ihl+2:ihl+4]) != port {
		return nil
	}
	n := int(binary.BigEndian.Uint16(p[ihl+4 : ihl+6]))
	if n < 8 || ihl+n > len(p) {
		return nil
	}
	return p[ihl+8 : ihl+n]
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// addAddress and addDefaultRoute configure the interface over netlink.
func addAddress(index int, ip net.IP, mask net.IPMask) error {
	ones, _ := mask.Size()
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(ones)
	binary.NativeEndian.PutUint32(msg[4:8], uint32(index))
	bcast := make(net.IP, 4)
	for i := range bcast {
		bcast[i] = ip.To4()[i] | ^mask[i]
	}
	msg = appendAttr(msg, unix.IFA_LOCAL, ip.To4())
	msg = appendAttr(msg, unix.IFA_ADDRESS, ip.To4())
	msg = appendAttr(msg, unix.IFA_BROADCAST, bcast)
	return netlinkRequest(unix.RTM_NEWADDR, msg)
}

func addDefaultRoute(index int, gw net.IP) error {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_UNIVERSE
	msg[7] = unix.RTN_UNICAST
	oif := make([]byte, 4)
	binary.NativeEndian.PutUint32(oif, uint32(index))
	msg = appendAttr(msg, unix.RTA_GATEWAY, gw.To4())
	msg = appendAttr(msg, unix.RTA_OIF, oif)
	return netlinkRequest(unix.RTM_NEWROUTE, msg)
}

func appendAttr(b []byte, typ uint16, data []byte) []byte {
	hdr := make([]byte, 4)
	binary.NativeEndian.PutUint16(hdr[0:2], uint16(4+len(data)))
	binary.NativeEndian.PutUint16(hdr[2:4], typ)
	b = append(b, hdr...)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// netlinkRequest sends a create request and waits for its acknowledgment.
func netlinkRequest(typ uint16, body []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	msg := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(body))
	binary.NativeEndian.PutUint32(msg[0:4], uint32(unix.SizeofNlMsghdr+len(body)))
	binary.NativeEndian.PutUint16(msg[4:6], typ)
	binary.NativeEndian.PutUint16(msg[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL)
	binary.NativeEndian.PutUint32(msg[8:12], 1) // sequence number
	msg = append(msg, body...)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	// The reply is an NLMSG_ERROR message whose error is 0 on success.
	b := buf[:n]
	if len(b) < unix.SizeofNlMsghdr+4 || binary.NativeEndian.Uint16(b[4:6]) != unix.NLMSG_ERROR {
		return errors.New("unexpected netlink reply")
	}
	if errno := int32(binary.NativeEndian.Uint32(b[unix.SizeofNlMsghdr:])); errno != 0 {
		if e := unix.Errno(-errno); e != unix.EEXIST {
			return e
		}
	}
	return nil
}

// staticLease is an address on the default vmnet NAT network
// (192.168.64.0/24, with the host at .1 serving DNS). The host gives
// each concurrent VM its own MAC address slot (02:61:69:63:00:slot); the
// address counts down from .254, away from where DHCP assigns them.
func staticLease(mac net.HardwareAddr) (*lease, error) {
	if len(mac) != 6 || !bytes.Equal(mac[:5], []byte{0x02, 0x61, 0x69, 0x63, 0x00}) || mac[5] > 100 {
		return nil, fmt.Errorf("no static address for MAC %s", mac)
	}
	gw := net.IPv4(192, 168, 64, 1).To4()
	return &lease{
		ip:     net.IPv4(192, 168, 64, 254-mac[5]).To4(),
		mask:   net.CIDRMask(24, 32),
		router: gw,
		dns:    []net.IP{gw},
	}, nil
}
