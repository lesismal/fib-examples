//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	stdtls "crypto/tls"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/lesismal/fib-examples/tls/dtls/session"
	fibtls "github.com/lesismal/fib/tls"
)

// echoServer is a DTLS echo for one client on a plain UDP socket, driven
// by the same Session as ../server. It returns its address and fingerprint.
func echoServer(t *testing.T) (string, string) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		t.Fatal(err)
	}
	config := &stdtls.Config{Certificates: []stdtls.Certificate{cert}, ClientAuth: stdtls.RequireAnyClientCert}
	var peer netip.AddrPort
	s, err := session.New(false, config, func(b []byte) { _, _ = pc.WriteToUDPAddrPort(b, peer) },
		fibtls.DTLSOptions{SRTPProfiles: session.SRTPProfiles},
		session.Callbacks{OnData: func(s *session.Session, b []byte) { _ = s.Write(b) }})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			peer = from
			s.HandleDatagram(b[:n])
		}
	}()
	return pc.LocalAddr().String(), fibtls.CertificateFingerprint(cert.Certificate[0])
}

func TestRunEchoes(t *testing.T) {
	addr, fp := echoServer(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-addr", addr, "-fingerprint", fp, "-n", "3"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"DTLS 1.2", "SRTP SRTP_AEAD_AES_128_GCM", "exported ", "echo: hello from fib #3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in\n%s", want, out.String())
		}
	}
}

func TestRunRefusesAnotherCertificate(t *testing.T) {
	addr, _ := echoServer(t)
	err := run(context.Background(), []string{"-addr", addr, "-fingerprint", "00:11"}, io.Discard)
	if err == nil {
		t.Fatal("accepted a certificate of another fingerprint")
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "no port"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
