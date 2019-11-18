package pubsub

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	pb "github.com/libp2p/go-libp2p-pubsub/pb"

	"github.com/libp2p/go-libp2p-core/helpers"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"

	ggio "github.com/gogo/protobuf/io"
)

type basicTracer struct {
	ch  chan struct{}
	mx  sync.Mutex
	buf []*pb.TraceEvent
}

func (t *basicTracer) Trace(evt *pb.TraceEvent) {
	t.mx.Lock()
	t.buf = append(t.buf, evt)
	t.mx.Unlock()

	select {
	case t.ch <- struct{}{}:
	default:
	}
}

func (t *basicTracer) Close() {
	close(t.ch)
}

// JSONTracer is a tracer that writes events to a file, encoded in ndjson.
type JSONTracer struct {
	basicTracer
	w io.WriteCloser
}

// NewJsonTracer creates a new JSONTracer writing traces to file.
func NewJSONTracer(file string) (*JSONTracer, error) {
	return OpenJSONTracer(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
}

// OpenJSONTracer creates a new JSONTracer, with explicit control of OpenFile flags and permissions.
func OpenJSONTracer(file string, flags int, perm os.FileMode) (*JSONTracer, error) {
	f, err := os.OpenFile(file, flags, perm)
	if err != nil {
		return nil, err
	}

	tr := &JSONTracer{w: f, basicTracer: basicTracer{ch: make(chan struct{}, 1)}}
	go tr.doWrite()

	return tr, nil
}

func (t *JSONTracer) doWrite() {
	var buf []*pb.TraceEvent
	enc := json.NewEncoder(t.w)
	for {
		_, ok := <-t.ch

		t.mx.Lock()
		tmp := t.buf
		t.buf = buf[:0]
		buf = tmp
		t.mx.Unlock()

		for i, evt := range buf {
			err := enc.Encode(evt)
			if err != nil {
				log.Errorf("error writing event trace: %s", err.Error())
			}
			buf[i] = nil
		}

		if !ok {
			t.w.Close()
			return
		}
	}
}

var _ EventTracer = (*JSONTracer)(nil)

// PBTracer is a tracer that writes events to a file, as delimited protobufs.
type PBTracer struct {
	basicTracer
	w io.WriteCloser
}

func NewPBTracer(file string) (*PBTracer, error) {
	return OpenPBTracer(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
}

// OpenPBTracer creates a new PBTracer, with explicit control of OpenFile flags and permissions.
func OpenPBTracer(file string, flags int, perm os.FileMode) (*PBTracer, error) {
	f, err := os.OpenFile(file, flags, perm)
	if err != nil {
		return nil, err
	}

	tr := &PBTracer{w: f, basicTracer: basicTracer{ch: make(chan struct{}, 1)}}
	go tr.doWrite()

	return tr, nil
}

func (t *PBTracer) doWrite() {
	var buf []*pb.TraceEvent
	w := ggio.NewDelimitedWriter(t.w)
	for {
		_, ok := <-t.ch

		t.mx.Lock()
		tmp := t.buf
		t.buf = buf[:0]
		buf = tmp
		t.mx.Unlock()

		for i, evt := range buf {
			err := w.WriteMsg(evt)
			if err != nil {
				log.Errorf("error writing event trace: %s", err.Error())
			}
			buf[i] = nil
		}

		if !ok {
			t.w.Close()
			return
		}
	}
}

var _ EventTracer = (*PBTracer)(nil)

const RemoteTracerProtoID = protocol.ID("/libp2p/pubsub/tracer/1.0.0")

// RemoteTracer is a tracer that sends trace events to a remote peer
type RemoteTracer struct {
	basicTracer
	ctx  context.Context
	host host.Host
	pi   peer.AddrInfo
}

// NewRemoteTracer constructs a RemoteTracer, tracing to the peer identified by pi
func NewRemoteTracer(ctx context.Context, host host.Host, pi peer.AddrInfo) (*RemoteTracer, error) {
	tr := &RemoteTracer{ctx: ctx, host: host, pi: pi, basicTracer: basicTracer{ch: make(chan struct{}, 1)}}
	go tr.doWrite()
	return tr, nil
}

func (t *RemoteTracer) doWrite() {
	var buf []*pb.TraceEvent

	s, err := t.openStream()
	if err != nil {
		log.Errorf("error opening remote tracer stream: %s", err.Error())
		return
	}

	var batch pb.TraceEventBatch

	gzipW := gzip.NewWriter(s)
	w := ggio.NewDelimitedWriter(gzipW)

	for {
		_, ok := <-t.ch

		// nil out the buffer to gc events when swapping buffers
		for i := range buf {
			buf[i] = nil
		}

		// wait a bit to accumulate a batch
		time.Sleep(time.Second)

		t.mx.Lock()
		tmp := t.buf
		t.buf = buf[:0]
		buf = tmp
		t.mx.Unlock()

		if len(buf) == 0 {
			goto end
		}

		batch.Batch = buf

		err = w.WriteMsg(&batch)
		if err != nil {
			log.Errorf("error writing trace event batch: %s", err)
			goto end
		}

		err = gzipW.Flush()
		if err != nil {
			log.Errorf("error flushin gzip stream: %s", err)
			goto end
		}

	end:
		if !ok {
			if err != nil {
				s.Reset()
			} else {
				gzipW.Close()
				helpers.FullClose(s)
			}
			return
		}

		if err != nil {
			s.Reset()
			s, err = t.openStream()
			if err != nil {
				log.Errorf("error opening remote tracer stream: %s", err.Error())
				return
			}

			gzipW.Reset(s)
		}
	}
}

func (t *RemoteTracer) connect() error {
	for {
		ctx, cancel := context.WithTimeout(t.ctx, time.Minute)
		err := t.host.Connect(ctx, t.pi)
		cancel()
		if err != nil {
			if t.ctx.Err() != nil {
				return err
			}

			// wait a minute and try again, to account for transient server downtime
			select {
			case <-time.After(time.Minute):
				continue
			case <-t.ctx.Done():
				return t.ctx.Err()
			}
		}

		return nil
	}
}

func (t *RemoteTracer) openStream() (network.Stream, error) {
	for {
		err := t.connect()
		if err != nil {
			return nil, err
		}

		ctx, cancel := context.WithTimeout(t.ctx, time.Minute)
		s, err := t.host.NewStream(ctx, t.pi.ID, RemoteTracerProtoID)
		cancel()
		if err != nil {
			if t.ctx.Err() != nil {
				return nil, err
			}

			// wait a minute and try again, to account for transient server downtime
			select {
			case <-time.After(time.Minute):
				continue
			case <-t.ctx.Done():
				return nil, t.ctx.Err()
			}
		}

		return s, nil
	}
}

var _ EventTracer = (*RemoteTracer)(nil)
