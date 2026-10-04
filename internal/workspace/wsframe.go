package workspace

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A minimal WebSocket client, just enough for the Kubernetes exec subresource.
//
// ponytail: hand-rolled framing instead of a WebSocket library. It is ~100
// lines because our side never sends data — no stdin means no payload masking,
// no fragmentation to produce, no write path to get wrong. We only read, and
// server-to-client frames are unmasked. Reach for coder/websocket if a PTY
// (which needs stdin) ever lands.
//
// Deliberately unsupported: sending data frames, and compression extensions
// (we never negotiate one).

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	// A single exec message is a chunk of command output. Anything past this
	// is a runaway, and buffering it would trade a hung command for a dead
	// gateway.
	maxWSMessage = 16 << 20
)

type wsConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
}

func newWSConn(rwc io.ReadWriteCloser) *wsConn {
	return &wsConn{rwc: rwc, br: bufio.NewReaderSize(rwc, 32<<10)}
}

func (c *wsConn) Close() error { return c.rwc.Close() }

// readMessage returns the next complete application message, reassembling
// fragments and answering control frames along the way. It returns io.EOF when
// the peer closes.
func (c *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	var started bool

	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}

		switch opcode {
		case opPing:
			// A pong is the only thing we ever write. Failing to answer would
			// have the API server drop a long-running command's stream.
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("websocket: new message before the previous one finished")
			}
			started = true
			msg = payload
		case opContinuation:
			if !started {
				return nil, errors.New("websocket: continuation without a start frame")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("websocket: unexpected opcode %#x", opcode)
		}

		if len(msg) > maxWSMessage {
			return nil, errors.New("websocket: message too large")
		}
		if fin {
			return msg, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		// A clean EOF here is the stream ending between frames.
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return
	}
	fin = hdr[0]&0x80 != 0
	// Any reserved bit set means an extension we never negotiated; carrying on
	// would silently misread the payload.
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	if opcode >= opClose {
		// Control frames carry status, not data: fragmenting one or making it
		// large is a protocol error, not something to accommodate.
		if !fin || length > 125 {
			return false, 0, nil, errors.New("websocket: malformed control frame")
		}
	}
	if length > maxWSMessage {
		return false, 0, nil, errors.New("websocket: frame too large")
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return
	}
	// A server has no business masking, but unmasking anyway costs nothing and
	// beats returning scrambled bytes.
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// writeFrame sends one unfragmented control frame. Client frames must be
// masked, per RFC 6455 — servers reject unmasked ones outright.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	buf := make([]byte, 0, 2+4+len(payload))
	buf = append(buf, 0x80|opcode, 0x80|byte(len(payload)))
	buf = append(buf, mask[:]...)
	for i, b := range payload {
		buf = append(buf, b^mask[i%4])
	}
	_, err := c.rwc.Write(buf)
	return err
}
