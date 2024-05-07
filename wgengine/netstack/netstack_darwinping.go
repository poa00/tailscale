// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package netstack

import (
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// sendICMPPingToIP sends an non-privileged ICMP (or ICMPv6) ping to dstIP.
// It waits for a reply from the destination before returning an optional error.
func (ns *Impl) sendICMPPingToIP(dstIP netip.Addr, timeout time.Duration) error {
	srcIP, srcIPErr := ns.getSrcIPForDstIP(dstIP)
	if srcIPErr != nil {
		ns.logf("sendICMPPingToIP: failed to get srcIP: %v", srcIPErr)
		return srcIPErr
	}

	var conn *icmp.PacketConn
	var listenErr error
	if dstIP.Is6() {
		conn, listenErr = icmp.ListenPacket("udp6", srcIP.String())
	} else {
		conn, listenErr = icmp.ListenPacket("udp4", srcIP.String())
	}
	if listenErr != nil {
		ns.logf("sendICMPPingToIP: failed to ListenPacket: %v", listenErr)
		return listenErr
	}
	defer conn.Close()

	setReadDeadlineErr := conn.SetReadDeadline(time.Now().Add(timeout))
	if setReadDeadlineErr != nil {
		ns.logf("sendICMPPingToIP: failed to SetReadDeadline: %v", setReadDeadlineErr)
		return setReadDeadlineErr
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	body := icmp.Echo{
		ID:   r.Intn(math.MaxUint16),
		Seq:  1,
		Data: []byte("1-800-NAT-BUST"),
	}
	var msg icmp.Message
	if dstIP.Is6() {
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Code: 0, Body: &body}
	} else {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Code: 0, Body: &body}
	}

	msgBytes, marshalErr := msg.Marshal(nil)
	if marshalErr != nil {
		ns.logf("sendICMPPingToIP: failed to marshal ICMP message: %v", marshalErr)
		return marshalErr
	}

	udpAddr := &net.UDPAddr{IP: dstIP.AsSlice(), Zone: dstIP.Zone()}
	_, writeErr := conn.WriteTo(msgBytes, udpAddr)
	if writeErr != nil {
		ns.logf("sendICMPPingToIP: failed to WriteTo: %v", writeErr)
		return writeErr
	}

	reply := make([]byte, 1500)
	n, peer, readErr := conn.ReadFrom(reply)
	if readErr != nil {
		ns.logf("sendICMPPingToIP: failed to ReadFrom: %v", readErr)
		return readErr
	}

	if peer.(*net.UDPAddr).IP.String() != dstIP.String() {
		return fmt.Errorf("sendICMPPingToIP: got ICMP reply from %s, but wanted %s", peer.String(), dstIP.String())
	}

	var proto int
	if dstIP.Is6() {
		proto = ipv6.ICMPTypeEchoRequest.Protocol()
	} else {
		proto = ipv4.ICMPTypeEcho.Protocol()
	}

	_, parseErr := icmp.ParseMessage(proto, reply[:n])
	if parseErr != nil {
		ns.logf("sendICMPPingToIP: failed to parse ICMP reply: %v", parseErr)
		return parseErr
	}

	return nil
}

// getSrcIPForDstIP dialsUDP dstIP at port 41642 and grabs the IP of the interface that
// the kernel used to reach dstIP. This saves us from having to read the routing table.
func (ns *Impl) getSrcIPForDstIP(dstIP netip.Addr) (netip.Addr, error) {
	var conn net.Conn
	var connErr error
	udpAddr := &net.UDPAddr{IP: dstIP.AsSlice(), Port: 41642, Zone: dstIP.Zone()}
	conn, connErr = net.DialUDP("udp", nil, udpAddr)
	if connErr != nil {
		ns.logf("failed to dial to dstIP %s: %v", dstIP.String(), connErr)
		return netip.Addr{}, connErr
	}
	defer conn.Close()
	return netip.ParseAddr(conn.LocalAddr().(*net.UDPAddr).IP.String())
}
