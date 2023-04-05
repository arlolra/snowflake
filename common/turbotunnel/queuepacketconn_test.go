package turbotunnel

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

type emptyAddr struct{}

func (_ emptyAddr) Network() string { return "empty" }
func (_ emptyAddr) String() string  { return "empty" }

type intAddr int

func (i intAddr) Network() string { return "int" }
func (i intAddr) String() string  { return fmt.Sprintf("%d", i) }

// Run with -benchmem to see memory allocations.
func BenchmarkQueueIncoming(b *testing.B) {
	conn := NewQueuePacketConn(emptyAddr{}, 1*time.Hour)
	defer conn.Close()

	b.ResetTimer()
	var p [500]byte
	for i := 0; i < b.N; i++ {
		conn.QueueIncoming(p[:], emptyAddr{})
	}
	b.StopTimer()
}

// BenchmarkWriteTo benchmarks the QueuePacketConn.WriteTo function.
func BenchmarkWriteTo(b *testing.B) {
	conn := NewQueuePacketConn(emptyAddr{}, 1*time.Hour)
	defer conn.Close()

	b.ResetTimer()
	var p [500]byte
	for i := 0; i < b.N; i++ {
		conn.WriteTo(p[:], emptyAddr{})
	}
	b.StopTimer()
}

// DiscardPacketConn is a net.PacketConn whose ReadFrom method block forever and
// whose WriteTo method discards whatever it is called with.
type DiscardPacketConn struct{}

func (_ DiscardPacketConn) ReadFrom(_ []byte) (int, net.Addr, error)  { select {} } // block forever
func (_ DiscardPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (_ DiscardPacketConn) Close() error                              { return nil }
func (_ DiscardPacketConn) LocalAddr() net.Addr                       { return emptyAddr{} }
func (_ DiscardPacketConn) SetDeadline(t time.Time) error             { return nil }
func (_ DiscardPacketConn) SetReadDeadline(t time.Time) error         { return nil }
func (_ DiscardPacketConn) SetWriteDeadline(t time.Time) error        { return nil }

// TranscriptPacketConn keeps a log of the []byte argument to every call to
// WriteTo.
type TranscriptPacketConn struct {
	Transcript [][]byte
	lock       sync.Mutex
	net.PacketConn
}

func NewTranscriptPacketConn(inner net.PacketConn) *TranscriptPacketConn {
	return &TranscriptPacketConn{
		PacketConn: inner,
	}
}

func (c *TranscriptPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	p2 := make([]byte, len(p))
	copy(p2, p)
	c.Transcript = append(c.Transcript, p2)

	return c.PacketConn.WriteTo(p, addr)
}

// Tests that QueuePacketConn.WriteTo is compatible with the way kcp-go uses
// PacketConn, allocating source buffers in a sync.Pool.
//
// https://bugs.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/40260
func TestQueuePacketConnWriteToKCP(t *testing.T) {
	// Start a goroutine to constantly exercise kcp UDPSession.tx, writing
	// packets with payload "XXXX".
	done := make(chan struct{}, 0)
	defer close(done)
	ready := make(chan struct{}, 0)
	go func() {
		var readyClose sync.Once
		defer readyClose.Do(func() { close(ready) })
		pconn := DiscardPacketConn{}
		defer pconn.Close()
	loop:
		for {
			select {
			case <-done:
				break loop
			default:
			}
			// Create a new UDPSession, send once, then discard the
			// UDPSession.
			conn, err := kcp.NewConn2(intAddr(2), nil, 0, 0, pconn)
			if err != nil {
				panic(err)
			}
			_, err = conn.Write([]byte("XXXX"))
			if err != nil {
				panic(err)
			}
			// Signal the main test to start once we have done one
			// iterator of this noisy loop.
			readyClose.Do(func() { close(ready) })
		}
	}()

	pconn := NewQueuePacketConn(emptyAddr{}, 1*time.Hour)
	defer pconn.Close()
	addr1 := intAddr(1)
	outgoing := pconn.OutgoingQueue(addr1)

	// Once the "XXXX" goroutine is started, repeatedly send a packet, wait,
	// then retrieve it and check whether it has changed since being sent.
	<-ready
	for i := 0; i < 10; i++ {
		transcript := NewTranscriptPacketConn(pconn)
		conn, err := kcp.NewConn2(addr1, nil, 0, 0, transcript)
		if err != nil {
			panic(err)
		}
		_, err = conn.Write([]byte("hello world"))
		if err != nil {
			panic(err)
		}

		err = conn.Close()
		if err != nil {
			panic(err)
		}

		// A sleep after the Write makes buffer reuse more likely.
		time.Sleep(100 * time.Millisecond)

		if len(transcript.Transcript) == 0 {
			panic("empty transcript")
		}

		for j, tr := range transcript.Transcript {
			p := <-outgoing
			// This test is meant to detect unsynchronized memory
			// changes, so freeze the slice we just read.
			p2 := make([]byte, len(p))
			copy(p2, p)
			if !bytes.Equal(p2, tr) {
				t.Fatalf("%d %d packet changed between send and recv\nsend: %+q\nrecv: %+q", i, j, tr, p2)
			}
		}
	}
}
