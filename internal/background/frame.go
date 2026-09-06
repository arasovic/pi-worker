package background

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const privateFrameLimit = 64 << 20 // 64 MiB

var errPrivateFrameTooLarge = errors.New("frame payload exceeds limit")

// writeFrame encodes a single frame to w as a big-endian uint32 length prefix
// followed by the raw payload bytes. The caller must supply a positive limit;
// payload or announced length above that limit is rejected before any write;
// the exact limit itself is valid. Returns nil on success,
// errPrivateFrameTooLarge when too large, or an I/O error. Writer may not be
// nil.
func writeFrame(w io.Writer, payload []byte, limit int) error {
	if w == nil {
		return fmt.Errorf("writer must not be nil")
	}
	if limit <= 0 {
		return fmt.Errorf("limit must be positive")
	}
	n := len(payload)
	if n > limit {
		return errPrivateFrameTooLarge
	}
	if uint64(n) > math.MaxUint32 {
		// uint32 length prefix cannot represent the payload size.
		return errPrivateFrameTooLarge
	}
	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(n))
	// Write the 4-byte prefix with a full-write loop.
	for offset := 0; offset < 4; {
		nw, err := w.Write(lenBytes[offset:])
		if err != nil {
			return err
		}
		if nw < 0 || nw > len(lenBytes[offset:]) {
			return io.ErrShortWrite
		}
		if nw == 0 {
			// zero-byte writes are treated as short-progress
			return io.ErrShortWrite
		}
		offset += nw
	}
	// Write the payload with a full-write loop.
	for offset := 0; offset < n; {
		nw, err := w.Write(payload[offset:])
		if err != nil {
			return err
		}
		if nw < 0 || nw > len(payload[offset:]) {
			return io.ErrShortWrite
		}
		if nw == 0 {
			return io.ErrShortWrite
		}
		offset += nw
	}
	return nil
}

// readFrame reads a single frame from r into a newly allocated byte slice.
// The wire format is a big-endian uint32 length prefix followed by that many
// payload bytes. The caller must supply a positive limit; announced length
// above it is rejected before allocation; the exact limit itself is valid.
// Returns nil on success for an empty payload, or an I/O error. Reader may not
// be nil.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("reader must not be nil")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}
	// Read exactly 4 bytes for the length prefix.
	var prefix [4]byte
	n, err := io.ReadFull(r, prefix[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			// EOF arrived before *any* prefix byte: preserve clean EOF.
			return nil, io.EOF
		}
		// Partial prefix or other error: surface the actual error with
		// context; do not mask it as io.ErrUnexpectedEOF.
		return nil, fmt.Errorf("read prefix: %w", err)
	}
	announcedLen := binary.BigEndian.Uint32(prefix[:])
	if uint64(announcedLen) > uint64(limit) {
		return nil, errPrivateFrameTooLarge
	}
	switch announcedLen {
	case 0:
		return nil, nil
	default:
		payload := make([]byte, announcedLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			// io.ReadFull returns io.ErrUnexpectedEOF on a partial read, and
			// the wrapped error keeps that distinction for the caller while
			// adding context.
			return nil, fmt.Errorf("read payload: %w", err)
		}
		return payload, nil
	}
}
