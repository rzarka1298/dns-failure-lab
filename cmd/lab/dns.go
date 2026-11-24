package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

type scenario string

const (
	healthy     scenario = "healthy"
	slow        scenario = "slow"
	timeout     scenario = "timeout"
	wrongAnswer scenario = "wrong_answer"
	staleAnswer scenario = "stale_answer"
	badRelease  scenario = "bad_release"
)

func validScenario(s scenario) bool {
	switch s {
	case healthy, slow, timeout, wrongAnswer, staleAnswer, badRelease:
		return true
	}
	return false
}

type dnsServer struct {
	conn   *net.UDPConn
	mode   atomic.Value
	goodIP net.IP
	badIP  net.IP
}

func startDNS() (*dnsServer, error) {
	return startDNSAt("127.0.0.1:0", net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 2))
}

func startDNSAt(address string, goodIP, badIP net.IP) (*dnsServer, error) {
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, err
	}
	if goodIP.To4() == nil || badIP.To4() == nil {
		_ = conn.Close()
		return nil, errors.New("DNS answers require IPv4 addresses")
	}
	d := &dnsServer{conn: conn, goodIP: append(net.IP(nil), goodIP.To4()...), badIP: append(net.IP(nil), badIP.To4()...)}
	d.mode.Store(healthy)
	go d.serve()
	return d, nil
}

func (d *dnsServer) address() string { return d.conn.LocalAddr().String() }
func (d *dnsServer) close()          { _ = d.conn.Close() }

func (d *dnsServer) serve() {
	buf := make([]byte, 512)
	for {
		n, peer, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		packet := append([]byte(nil), buf[:n]...)
		go d.answer(packet, peer)
	}
}

func (d *dnsServer) answer(packet []byte, peer *net.UDPAddr) {
	mode := d.mode.Load().(scenario)
	if mode == timeout {
		return
	}
	if mode == slow {
		time.Sleep(350 * time.Millisecond)
	}
	response := makeDNSResponseWithIPs(packet, mode, d.goodIP, d.badIP)
	if response != nil {
		_, _ = d.conn.WriteToUDP(response, peer)
	}
}

func makeDNSResponse(packet []byte, mode scenario) []byte {
	return makeDNSResponseWithIPs(packet, mode, net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 2))
}

func makeDNSResponseWithIPs(packet []byte, mode scenario, goodIP, badIP net.IP) []byte {
	name, end, ok := parseQuestion(packet)
	if !ok {
		return nil
	}
	// Go's resolver may ask for both A and AAAA. Answer AAAA with no records.
	qtype := binary.BigEndian.Uint16(packet[end-4 : end-2])
	response := append([]byte(nil), packet[:end]...)
	response[2], response[3] = 0x81, 0x80
	for i := 6; i < 12; i++ {
		response[i] = 0
	}
	if name != "api.lab.test" {
		response[3] = 0x83
	}
	if name == "api.lab.test" && qtype == 1 {
		response[7] = 1
		ip := goodIP.To4()
		if mode == wrongAnswer {
			ip = badIP.To4()
		}
		answer := []byte{0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4}
		if mode == wrongAnswer {
			// The former answer remains valid in a resolver cache for 30 seconds.
			binary.BigEndian.PutUint32(answer[6:10], 30)
		}
		response = append(response, answer...)
		response = append(response, ip...)
	}
	return response
}

func parseQuestion(p []byte) (string, int, bool) {
	if len(p) < 17 || binary.BigEndian.Uint16(p[4:6]) != 1 {
		return "", 0, false
	}
	var labels []string
	i := 12
	for {
		if i >= len(p) {
			return "", 0, false
		}
		length := int(p[i])
		i++
		if length == 0 {
			break
		}
		if length > 63 || i+length > len(p) {
			return "", 0, false
		}
		labels = append(labels, string(p[i:i+length]))
		i += length
	}
	if i+4 > len(p) {
		return "", 0, false
	}
	return strings.ToLower(strings.Join(labels, ".")), i + 4, true
}

func labResolver(address string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		if network != "udp" && network != "tcp" {
			return nil, errors.New("unsupported DNS transport")
		}
		return (&net.Dialer{}).DialContext(ctx, "udp", address)
	}}
}
