package stream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Box is a top-level ISO-BMFF box read from FFmpeg's fragmented MP4 output.
type Box struct {
	Type string
	Data []byte // full box including the 8-byte header
}

// readBox reads one top-level box. FFmpeg's fragmented output never uses
// 64-bit sizes for top-level boxes at our segment sizes, but we handle them anyway.
func readBox(r io.Reader) (Box, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Box{}, err
	}
	size := uint64(binary.BigEndian.Uint32(hdr[:4]))
	typ := string(hdr[4:8])
	headerLen := uint64(8)
	var ext [8]byte
	if size == 1 {
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Box{}, err
		}
		size = binary.BigEndian.Uint64(ext[:])
		headerLen = 16
	}
	if size < headerLen {
		return Box{}, fmt.Errorf("invalid mp4 box %q size %d", typ, size)
	}
	if size > maxBoxSize {
		return Box{}, fmt.Errorf("mp4 box %q too large (%d bytes)", typ, size)
	}
	buf := make([]byte, size)
	copy(buf, hdr[:])
	if headerLen == 16 {
		copy(buf[8:], ext[:])
	}
	if _, err := io.ReadFull(r, buf[headerLen:]); err != nil {
		return Box{}, err
	}
	return Box{Type: typ, Data: buf}, nil
}

const maxBoxSize = 32 << 20

var errNoCodec = errors.New("no supported video codec found in init segment")

// codecString derives an RFC 6381 codec string from the moov box so the
// browser can construct a matching MediaSource SourceBuffer.
func codecString(moov []byte) (string, error) {
	i := bytes.Index(moov, []byte("avcC"))
	if i < 0 || i+8 > len(moov) {
		return "", errNoCodec
	}
	// avcC payload: configurationVersion, AVCProfileIndication, profile_compatibility, AVCLevelIndication
	p := moov[i+4:]
	return fmt.Sprintf("avc1.%02x%02x%02x", p[1], p[2], p[3]), nil
}
