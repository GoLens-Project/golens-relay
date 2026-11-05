package main

// Tests for the proxy data path — the EOF-closure contract this repo
// exists to pin: handle() must copy bytes both ways and deliver each
// side's end-of-stream as a TCP half-close (CloseWrite), never a full
// close, so the peer that is still writing can finish. Concretely: an
// upstream that answers only after reading the request to EOF must still
// get its response back to a client that already half-closed, and an
// upstream that closes must still deliver its bytes plus EOF to the
// client.

import (
	"io"
	"log"
	"net"
	"testing"
	"time"
)

const (
	testRequest  = "ping"
	testResponse = "pong"
)

// proxyConn dials a fresh connection through handle() to the given
// upstream address and returns the client end — the same shape serve uses
// for each accepted connection. The returned cleanup closes the client and
// the front listener.
func proxyConn(t *testing.T, upstream string) (client net.Conn, cleanup func()) {
	t.Helper()
	frontLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen front: %v", err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := frontLn.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err = net.Dial("tcp", frontLn.Addr().String())
	if err != nil {
		frontLn.Close()
		t.Fatalf("dial front: %v", err)
	}
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(readDeadline):
		client.Close()
		frontLn.Close()
		t.Fatalf("front accept timed out")
	}
	go handle(conn, upstream)
	return client, func() {
		client.Close()
		frontLn.Close()
	}
}

// upstreamListener starts a stand-in upstream server that runs fn for its
// first accepted connection and returns its dial address.
func upstreamListener(t *testing.T, fn func(up net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			fn(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func TestHandle(t *testing.T) {
	t.Run("request EOF half-closes to upstream, response still flows back", func(t *testing.T) {
		up := upstreamListener(t, func(up net.Conn) {
			defer up.Close()
			up.SetReadDeadline(time.Now().Add(readDeadline))
			got, err := io.ReadAll(up)
			if err != nil {
				t.Errorf("upstream read to EOF: %v", err)
				return
			}
			if string(got) != testRequest {
				t.Errorf("upstream got %q, want %q", got, testRequest)
				return
			}
			// The upstream answers only after seeing EOF. The write must
			// succeed on the still-open connection: a full close on the
			// client side's EOF would break here instead.
			up.SetWriteDeadline(time.Now().Add(readDeadline))
			if n, err := up.Write([]byte(testResponse)); err != nil || n != len(testResponse) {
				t.Errorf("upstream write after EOF: n=%d, err=%v", n, err)
			}
		})
		client, cleanup := proxyConn(t, up)
		defer cleanup()

		client.SetDeadline(time.Now().Add(readDeadline))
		if _, err := client.Write([]byte(testRequest)); err != nil {
			t.Fatalf("client write: %v", err)
		}
		if err := client.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("client half-close: %v", err)
		}
		got, err := io.ReadAll(client)
		if err != nil {
			t.Fatalf("client read after its own half-close: %v", err)
		}
		if string(got) != testResponse {
			t.Fatalf("client got %q, want %q", got, testResponse)
		}
	})

	t.Run("upstream EOF reaches the client after the data", func(t *testing.T) {
		up := upstreamListener(t, func(up net.Conn) {
			up.Write([]byte(testResponse))
			up.Close()
		})
		client, cleanup := proxyConn(t, up)
		defer cleanup()

		client.SetReadDeadline(time.Now().Add(readDeadline))
		got, err := io.ReadAll(client)
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		if string(got) != testResponse {
			t.Fatalf("client got %q, want %q", got, testResponse)
		}
	})

	t.Run("unreachable upstream closes the client connection", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("probe listen: %v", err)
		}
		dead := ln.Addr().String()
		ln.Close()

		// The dial failure is expected to be logged; keep it out of the
		// test output.
		saved := log.Writer()
		log.SetOutput(io.Discard)
		defer log.SetOutput(saved)

		client, cleanup := proxyConn(t, dead)
		defer cleanup()

		client.SetReadDeadline(time.Now().Add(readDeadline))
		buf := make([]byte, 64)
		n, err := client.Read(buf)
		if n != 0 || err != io.EOF {
			t.Fatalf("client read after upstream dial failure: n=%d, err=%v, want 0, io.EOF", n, err)
		}
	})
}
