package outbound

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type fakeMX struct {
	hosts []*net.MX
	err   error
}

func (f fakeMX) LookupMX(context.Context, string) ([]*net.MX, error) { return f.hosts, f.err }
func (f fakeMX) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
}

func TestDirectSMTPOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, rcpt, data     string
		permanent, uncertain bool
	}{
		{"accepted even if QUIT disconnects", "250 ok", "250 accepted", false, false},
		{"permanent rejection", "550 no such user", "", true, false},
		{"temporary rejection", "450 busy", "", false, false},
		{"uncertain after DATA", "250 ok", "", false, true},
		{"explicit DATA rejection", "250 ok", "554 rejected", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan struct{})
			received := make(chan string, 1)
			go func() {
				defer close(done)
				defer server.Close()
				server.SetDeadline(time.Now().Add(3 * time.Second))
				reader := textproto.NewReader(bufio.NewReader(server))
				io.WriteString(server, "220 mx.test ESMTP\r\n")
				for {
					line, err := reader.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO "):
						io.WriteString(server, "250-mx.test\r\n250 SIZE 10000\r\n")
					case strings.HasPrefix(line, "MAIL FROM:"):
						io.WriteString(server, "250 ok\r\n")
					case strings.HasPrefix(line, "RCPT TO:"):
						io.WriteString(server, test.rcpt+"\r\n")
					case line == "DATA":
						io.WriteString(server, "354 send\r\n")
						body, err := io.ReadAll(reader.DotReader())
						if err != nil {
							return
						}
						received <- string(body)
						if test.data == "" {
							return
						}
						io.WriteString(server, test.data+"\r\n")
					case line == "QUIT":
						return
					default:
						io.WriteString(server, "500 unexpected\r\n")
					}
				}
			}()
			transport := &DirectTransport{Hostname: "mail.sender.test", Resolver: fakeMX{hosts: []*net.MX{{Host: "mx.recipient.test.", Pref: 10}}}, Dial: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
			raw := []byte("From: sender@sender.test\r\nTo: receiver@recipient.test\r\n\r\nhello\r\n.dot\r\n")
			err := transport.Deliver(context.Background(), "bounce@sender.test", "receiver@recipient.test", raw)
			if strings.HasPrefix(test.name, "accepted") {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var delivery *DeliveryError
				if !errors.As(err, &delivery) || delivery.Permanent != test.permanent || delivery.Uncertain != test.uncertain {
					t.Fatalf("outcome=%+v", err)
				}
			}
			<-done
			if test.data != "" || test.uncertain {
				select {
				case body := <-received:
					if !strings.Contains(body, ".dot") {
						t.Fatal(body)
					}
				default:
					t.Fatal("no DATA observed")
				}
			}
		})
	}
}

func TestMXNullAndPrivateAddresses(t *testing.T) {
	transport := &DirectTransport{Hostname: "mail.sender.test", Resolver: fakeMX{hosts: []*net.MX{{Host: "."}}}}
	err := transport.Deliver(context.Background(), "a@sender.test", "b@recipient.test", nil)
	var failure *DeliveryError
	if !errors.As(err, &failure) || !failure.Permanent || !strings.Contains(failure.Reason, "Null MX") {
		t.Fatal(err)
	}
	transport.Resolver = fakeMX{hosts: []*net.MX{{Host: "private.test."}}}
	err = transport.Deliver(context.Background(), "a@sender.test", "b@recipient.test", nil)
	if !errors.As(err, &failure) || !failure.Permanent {
		t.Fatal(err)
	}
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "::ffff:127.0.0.1", "192.0.2.1"} {
		if publicIP(net.ParseIP(value)) {
			t.Error("non-public permitted", value)
		}
	}
	if !publicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
}

func TestSMTPRejectsAdvertisedTLSFailure(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		server.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(server)
		io.WriteString(server, "220 mx.test\r\n")
		reader.ReadString('\n')
		io.WriteString(server, "250-mx.test\r\n250 STARTTLS\r\n")
		line, _ := reader.ReadString('\n')
		if !bytes.HasPrefix([]byte(line), []byte("STARTTLS")) {
			return
		}
		io.WriteString(server, "220 start TLS\r\n")
	}()
	err := (&DirectTransport{Hostname: "mail.sender.test"}).send(context.Background(), client, "mx.test", "a@sender.test", "b@recipient.test", nil)
	var delivery *DeliveryError
	if !errors.As(err, &delivery) || delivery.Permanent || delivery.Uncertain || !strings.Contains(delivery.Reason, "TLS") {
		t.Fatal(err)
	}
	<-done
}

type mixedMXResolver struct{}

func (mixedMXResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Host: "temporary.test.", Pref: 10}, {Host: "missing.test.", Pref: 20}}, nil
}
func (mixedMXResolver) LookupIPAddr(_ context.Context, name string) ([]net.IPAddr, error) {
	if name == "temporary.test" {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	return nil, &net.DNSError{Err: "not found", IsNotFound: true, Name: name}
}
func TestTemporaryMXFailureIsNotOverwrittenByMissingFallback(t *testing.T) {
	transport := &DirectTransport{Hostname: "mail.sender.test", Resolver: mixedMXResolver{}, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("temporary network outage")
	}}
	err := transport.Deliver(context.Background(), "a@sender.test", "b@recipient.test", nil)
	var outcome *DeliveryError
	if !errors.As(err, &outcome) || outcome.Permanent || outcome.Uncertain {
		t.Fatal("temporary MX error became permanent", err)
	}
}
