package background

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"
)

// oneByteReader reads at most one byte per Read call.
type oneByteReader struct {
	data []byte
	pos  int
}

func newOneByteReaderFrom(data []byte) *oneByteReader {
	cp := make([]byte, len(data))
	copy(cp, data)
	return &oneByteReader{data: cp}
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// prefixReader yields only a fixed number of bytes then EOFs.
type prefixReader struct {
	data []byte
	pos  int
}

func (r *prefixReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// zeroNilWriter always returns (0, nil).
type zeroNilWriter struct{}

func (w *zeroNilWriter) Write(p []byte) (int, error) {
	return 0, nil
}

// errorWriter fails on the first call.
type errorWriter struct {
	err error
}

func (w *errorWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

// eofReader immediately returns EOF.
type eofReader struct{}

func (r *eofReader) Read(p []byte) (int, error) {
	return 0, io.EOF
}

// shortReader reads len(data) bytes then returns (0, sentinel).
type shortReader struct {
	data     []byte
	pos      int
	sentinel error
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.sentinel
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// longWriteWriter claims to write more bytes than provided.
type longWriteWriter struct{}

func (w *longWriteWriter) Write(p []byte) (int, error) {
	return len(p) + 1, nil
}

func TestFrameCodec(t *testing.T) {
	t.Run("exact_bytes", func(t *testing.T) {
		payload := []byte{0xaa, 0xbb, 0xcc}
		var buf bytes.Buffer
		if err := writeFrame(&buf, payload, 64<<20); err != nil {
			t.Fatal(err)
		}
		want := []byte{0x00, 0x00, 0x00, 0x03, 0xaa, 0xbb, 0xcc}
		got := buf.Bytes()
		if !bytes.Equal(got, want) {
			t.Errorf("wire = %v, want %v", got, want)
		}
	})

	t.Run("empty_roundtrip", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeFrame(&buf, nil, 64<<20); err != nil {
			t.Fatal(err)
		}
		got, err := readFrame(&buf, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil || len(got) != 0 {
			t.Fatalf("expected nil/empty payload, got %v", got)
		}
	})

	t.Run("normal_roundtrip", func(t *testing.T) {
		payload := []byte("hello world")
		var buf bytes.Buffer
		if err := writeFrame(&buf, payload, 64<<20); err != nil {
			t.Fatal(err)
		}
		got, err := readFrame(&buf, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %v, want %v", got, payload)
		}
	})

	t.Run("two_sequential_frames", func(t *testing.T) {
		p1 := []byte("first")
		p2 := []byte{0xff}
		var buf bytes.Buffer
		if err := writeFrame(&buf, p1, 64<<20); err != nil {
			t.Fatal(err)
		}
		if err := writeFrame(&buf, p2, 64<<20); err != nil {
			t.Fatal(err)
		}
		g1, err := readFrame(&buf, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(g1, p1) {
			t.Errorf("frame 1: got %v, want %v", g1, p1)
		}
		g2, err := readFrame(&buf, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(g2, p2) {
			t.Errorf("frame 2: got %v, want %v", g2, p2)
		}
	})

	t.Run("at_exact_limit", func(t *testing.T) {
		const lim = 8
		payload := make([]byte, lim)
		for i := range payload {
			payload[i] = byte(i)
		}
		var buf bytes.Buffer
		if err := writeFrame(&buf, payload, lim); err != nil {
			t.Fatal("write ok:", err)
		}
		got, err := readFrame(&buf, lim)
		if err != nil {
			t.Fatal("read ok:", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %v, want %v", got, payload)
		}
	})

	t.Run("over_limit", func(t *testing.T) {
		const lim = 8
		payload := make([]byte, lim+1) // above limit → reject
		if err := writeFrame(&bytes.Buffer{}, payload, lim); err != errPrivateFrameTooLarge {
			t.Fatalf("wanted errPrivateFrameTooLarge, got %v", err)
		}
		announced := make([]byte, 4)
		announced[0] = byte(lim + 1) // announced length == limit+1 → reject
		r := &prefixReader{data: announced}
		if _, err := readFrame(r, lim); err != errPrivateFrameTooLarge {
			t.Fatalf("wanted errPrivateFrameTooLarge, got %v", err)
		}
	})

	t.Run("fragmented_writer_one_byte", func(t *testing.T) {
		payload := []byte("test payload")
		var collected []byte
		fragWriter := &oneByteAccum{dst: &collected}
		if err := writeFrame(fragWriter, payload, 64<<20); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		buf.Write(collected)
		got, err := readFrame(&buf, 64<<20)
		if err != nil {
			t.Fatal("round-trip read:", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %v, want %v", got, payload)
		}
	})

	t.Run("fragmented_reader_one_byte", func(t *testing.T) {
		payload := []byte("frag reader")
		var buf bytes.Buffer
		if err := writeFrame(&buf, payload, 64<<20); err != nil {
			t.Fatal("write:", err)
		}
		data := buf.Bytes()
		fragReader := newOneByteReaderFrom(data)
		got, err := readFrame(fragReader, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %v, want %v", got, payload)
		}
	})

	t.Run("nil_writer_returns_error", func(t *testing.T) {
		err := writeFrame(nil, []byte("x"), 8)
		if err == nil {
			t.Fatal("wanted non-nil error for nil writer")
		}
	})

	t.Run("writer_zero_nil", func(t *testing.T) {
		zw := &zeroNilWriter{}
		err := writeFrame(zw, []byte("x"), 64<<20)
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("wanted ErrShortWrite, got %v", err)
		}
	})

	t.Run("writer_reports_too_many_bytes", func(t *testing.T) {
		w := &longWriteWriter{}
		err := writeFrame(w, []byte("test"), 64<<20)
		if err != io.ErrShortWrite {
			t.Fatalf("wanted ErrShortWrite, got %v", err)
		}
	})

	t.Run("writer_error_propagated", func(t *testing.T) {
		testErr := io.ErrNoProgress
		werr := &errorWriter{err: testErr}
		err := writeFrame(werr, []byte("x"), 64<<20)
		if err != testErr {
			t.Fatalf("wanted %v, got %v", testErr, err)
		}
	})

	t.Run("clean_eof_before_anything", func(t *testing.T) {
		eofReader := &eofReader{}
		_, err := readFrame(eofReader, 64<<20)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("wanted io.EOF, got %v", err)
		}
	})

	t.Run("partial_prefix_returns_sentinel", func(t *testing.T) {
		sentinel := fmt.Errorf("sentinel error")
		data := []byte{0x00, 0x01} // only 2 of 4 prefix bytes
		r := &shortReader{data: data, sentinel: sentinel}
		_, err := readFrame(r, 64<<20)
		if !errors.Is(err, sentinel) {
			t.Fatalf("wanted sentinel, got %v", err)
		}
	})

	t.Run("partial_payload_returns_sentinel", func(t *testing.T) {
		sentinel := fmt.Errorf("payload sentinel")
		// announce 8-byte payload, provide only 3 bytes then sentinel
		data := append([]byte{0x00, 0x00, 0x00, 0x08}, 0xAB, 0xCD, 0xEF)
		r := &shortReader{data: data, sentinel: sentinel}
		_, err := readFrame(r, 64<<20)
		if !errors.Is(err, sentinel) {
			t.Fatalf("wanted sentinel, got %v", err)
		}
	})

	t.Run("partial_prefix_wraps_unexpected_EOF", func(t *testing.T) {
		r := &prefixReader{data: []byte{0x00, 0x01}} // only 2 of 4 prefix bytes
		_, err := readFrame(r, 64<<20)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("wanted ErrUnexpectedEOF, got %v", err)
		}
	})

	t.Run("partial_payload_wraps_unexpected_EOF", func(t *testing.T) {
		prefix := make([]byte, 4)
		prefix[0] = 0x00
		prefix[3] = 0x04
		shortPayload := []byte{0xDE, 0xAD}
		allData := append(prefix, shortPayload...)
		r := &bytesReader{data: allData}
		_, err := readFrame(r, 64<<20)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("wanted ErrUnexpectedEOF, got %v", err)
		}
	})

	t.Run("oversized_announced_before_allocation", func(t *testing.T) {
		const smallLim = 8
		prefix := make([]byte, 4)
		prefix[3] = byte(smallLim + 1) // announce size == limit+1 → reject
		r := &prefixReader{data: prefix}
		_, err := readFrame(r, smallLim)
		if err != errPrivateFrameTooLarge {
			t.Fatalf("wanted errPrivateFrameTooLarge, got %v", err)
		}
	})

	t.Run("zero_write_limit", func(t *testing.T) {
		err := writeFrame(&bytes.Buffer{}, nil, 0)
		if err == nil {
			t.Fatal("wanted non-nil error for zero write limit")
		}
	})

	t.Run("negative_read_limit", func(t *testing.T) {
		_, err := readFrame(bytes.NewReader(nil), -1)
		if err == nil {
			t.Fatal("wanted non-nil error for negative read limit")
		}
	})

	t.Run("negative_write_limit", func(t *testing.T) {
		err := writeFrame(&bytes.Buffer{}, nil, -1)
		if err == nil {
			t.Fatal("wanted non-nil error for negative write limit")
		}
	})

	t.Run("nil_reader_zero_limit", func(t *testing.T) {
		_, err := readFrame(nil, 0)
		if err == nil {
			t.Fatal("wanted non-nil error for nil reader / zero limit")
		}
	})

	t.Run("math.MaxUint32_oversized_before_allocation", func(t *testing.T) {
		// Announce math.MaxUint32 but keep limit tiny.
		prefix := make([]byte, 4)
		binary.BigEndian.PutUint32(prefix, math.MaxUint32)
		trackReader := &countingReader{base: &prefixReader{data: prefix}}
		if _, err := readFrame(trackReader, 8); err != errPrivateFrameTooLarge {
			t.Fatalf("wanted errPrivateFrameTooLarge, got %v", err)
		}
		if trackReader.count != 1 {
			t.Errorf("expected 1 Read call for prefix, got %d", trackReader.count)
		}
	})
}

// oneByteAccum writes exactly one byte per Write call, appending to dst.
type oneByteAccum struct {
	dst *[]byte
	pos int
}

func (w *oneByteAccum) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	*w.dst = append(*w.dst, p[0])
	w.pos++
	return 1, nil
}

// countingReader wraps another Reader and counts Read calls.
type countingReader struct {
	base  io.Reader
	count int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.count++
	return r.base.Read(p)
}

// bytesReader wraps []byte for fragmented reads (chunks up to reader's choice).
type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
