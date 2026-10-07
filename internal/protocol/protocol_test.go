package protocol_test

import (
	"bytes"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/tunnel"
)

func TestControlRequest(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()

	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	client, server := protocol.NewControl(clientConn), protocol.NewControl(serverConn)
	bind := &protocol.Bind{Name: "web", Proto: tunnel.HTTP, Subdomain: "myapp"}

	go func() {
		m, err := server.Recv()
		if err != nil || m.Kind != protocol.KindBind || !reflect.DeepEqual(m.Bind, bind) {
			_ = server.Send(&protocol.Message{Kind: protocol.KindError, Error: "bad bind"})

			return
		}

		_ = server.Send(&protocol.Message{Kind: protocol.KindBound, Bound: &protocol.Bound{Name: "web", URL: "http://myapp.example.com"}})

		if _, err := server.Recv(); err == nil {
			_ = server.Send(&protocol.Message{Kind: protocol.KindError, Error: "subdomain taken"})
		}
	}()

	reply, err := client.Request(&protocol.Message{Kind: protocol.KindBind, Bind: bind}, protocol.KindBound)
	if err != nil {
		t.Fatalf("Request() unexpected error: %v", err)
	}

	if reply.Bound.URL != "http://myapp.example.com" {
		t.Errorf("Request() URL = %q", reply.Bound.URL)
	}

	_, err = client.Request(&protocol.Message{Kind: protocol.KindBind, Bind: bind}, protocol.KindBound)

	var remote *protocol.RemoteError
	if !errors.As(err, &remote) || remote.Message != "subdomain taken" {
		t.Errorf("Request() error = %v, want RemoteError", err)
	}
}

func TestStreamHeader(t *testing.T) {
	t.Parallel()

	tests := []protocol.StreamHeader{
		{Tunnel: "web", RemoteAddr: "203.0.113.7:51234"},
		{Tunnel: "db"},
		{},
	}

	for _, want := range tests {
		t.Run(want.Tunnel, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := protocol.WriteStreamHeader(&buf, want); err != nil {
				t.Fatalf("WriteStreamHeader() error: %v", err)
			}

			buf.WriteString("payload")

			got, err := protocol.ReadStreamHeader(&buf)
			if err != nil {
				t.Fatalf("ReadStreamHeader() error: %v", err)
			}

			if got != want {
				t.Errorf("ReadStreamHeader() = %+v, want %+v", got, want)
			}

			if rest := buf.String(); rest != "payload" {
				t.Errorf("header consumed payload, remaining %q", rest)
			}
		})
	}
}
