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
