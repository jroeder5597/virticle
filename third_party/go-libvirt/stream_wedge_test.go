package libvirt

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/socket"
	"github.com/stretchr/testify/assert"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("consumer gone") }

type blockReader struct{ release chan struct{} }

func (b blockReader) Read([]byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// TestStreamConsumerFailureDoesNotWedgeConnection: when the Writer given to a
// stream call fails (browser went away) while libvirtd keeps sending, the
// router must not stay blocked holding cmux. register/deregister, and so every
// later call on the connection, have to keep working.
func TestStreamConsumerFailureDoesNotWedgeConnection(t *testing.T) {
	l, srv := setupConsoleTest(t)
	release := make(chan struct{})
	defer close(release)

	go func() {
		hdr, _, err := srv.readPacket()
		if err != nil {
			return
		}
		serial, proc := hdr.Serial, hdr.Procedure
		if srv.writePacket(serial, proc, socket.Reply, socket.StatusOK, nil) != nil {
			return
		}
		go func() { // swallow whatever the client sends back
			for {
				if _, _, err := srv.readPacket(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < 500; i++ {
			if srv.writePacket(serial, proc, socket.Stream, socket.StatusContinue, []byte("boot output\r\n")) != nil {
				return
			}
		}
	}()

	// Unpatched, the call itself deadlocks in its deferred deregister.
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := l.DomainOpenConsoleBidirectional(Domain{}, nil,
			io.MultiReader(strings.NewReader("x"), blockReader{release}), failWriter{}, 0)
		assert.Error(t, err)
		c := make(chan response)
		l.register(4242, c)
		l.cmux.Lock()
		l.deregister(4242)
		l.cmux.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection wedged: the router holds cmux after the stream consumer failed")
	}
}

type collectWriter struct{ got chan []byte }

func (c collectWriter) Write(p []byte) (int, error) {
	c.got <- append([]byte(nil), p...)
	return len(p), nil
}

// TestStreamDataBeforeReply: libvirtd can send stream data before the reply
// to the call that opened the stream. The data must reach the writer in
// order, and the reply (empty, status OK) must not be taken for the end of
// the stream.
func TestStreamDataBeforeReply(t *testing.T) {
	l, srv := setupConsoleTest(t)
	release := make(chan struct{})
	finish := make(chan struct{})

	go func() {
		hdr, _, err := srv.readPacket()
		if err != nil {
			return
		}
		serial, proc := hdr.Serial, hdr.Procedure
		srv.writePacket(serial, proc, socket.Stream, socket.StatusContinue, []byte("a"))
		srv.writePacket(serial, proc, socket.Stream, socket.StatusContinue, []byte("b"))
		srv.writePacket(serial, proc, socket.Reply, socket.StatusOK, nil)
		srv.writePacket(serial, proc, socket.Stream, socket.StatusContinue, []byte("c"))
		go func() {
			for {
				if _, _, err := srv.readPacket(); err != nil {
					return
				}
			}
		}()
		<-finish
		srv.writePacket(serial, proc, socket.Stream, socket.StatusOK, nil)
	}()

	got := make(chan []byte, 8)
	done := make(chan error, 1)
	go func() {
		done <- l.DomainOpenConsoleBidirectional(Domain{}, nil, blockReader{release}, collectWriter{got}, 0)
	}()
	var all []byte
	for len(all) < 3 {
		select {
		case p := <-got:
			all = append(all, p...)
		case err := <-done:
			t.Fatalf("the console ended after %q (err %v): the reply was taken for the end of the stream", all, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("only %q arrived", all)
		}
	}
	assert.Equal(t, "abc", string(all))
	close(finish)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the console did not end on the end-of-stream packet")
	}
	close(release)
}
