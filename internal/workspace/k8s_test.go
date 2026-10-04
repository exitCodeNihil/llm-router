package workspace

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// rwc glues a canned server-to-client byte stream to a buffer capturing what we
// write back, so the frame reader can be exercised without a network.
type rwc struct {
	r io.Reader
	w bytes.Buffer
}

func (c *rwc) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *rwc) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *rwc) Close() error                { return nil }

// serverFrame builds an unmasked frame, which is what a server always sends.
func serverFrame(fin bool, opcode byte, payload []byte) []byte {
	var b []byte
	first := opcode
	if fin {
		first |= 0x80
	}
	b = append(b, first)
	switch n := len(payload); {
	case n < 126:
		b = append(b, byte(n))
	case n < 1<<16:
		b = append(b, 126, 0, 0)
		binary.BigEndian.PutUint16(b[len(b)-2:], uint16(n))
	default:
		b = append(b, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(b[len(b)-8:], uint64(n))
	}
	return append(b, payload...)
}

func TestWSReadMessage(t *testing.T) {
	// A whole message in one frame.
	c := newWSConn(&rwc{r: bytes.NewReader(serverFrame(true, opBinary, []byte("\x01hello")))})
	msg, err := c.readMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "\x01hello" {
		t.Errorf("msg = %q", msg)
	}

	// Fragmented: the API server splits large output, and dropping the tail
	// would silently truncate a command's stdout.
	stream := bytes.Join([][]byte{
		serverFrame(false, opBinary, []byte("\x01one ")),
		serverFrame(false, opContinuation, []byte("two ")),
		serverFrame(true, opContinuation, []byte("three")),
	}, nil)
	c = newWSConn(&rwc{r: bytes.NewReader(stream)})
	msg, err = c.readMessage()
	if err != nil {
		t.Fatalf("fragmented read: %v", err)
	}
	if string(msg) != "\x01one two three" {
		t.Errorf("fragmented msg = %q", msg)
	}

	// A ping mid-message must be answered and must not corrupt the message.
	conn := &rwc{r: bytes.NewReader(bytes.Join([][]byte{
		serverFrame(false, opBinary, []byte("\x01a")),
		serverFrame(true, opPing, []byte("hi")),
		serverFrame(true, opContinuation, []byte("b")),
	}, nil))}
	c = newWSConn(conn)
	msg, err = c.readMessage()
	if err != nil {
		t.Fatalf("ping read: %v", err)
	}
	if string(msg) != "\x01ab" {
		t.Errorf("msg across ping = %q", msg)
	}
	// The pong must be masked — servers reject unmasked client frames.
	sent := conn.w.Bytes()
	if len(sent) < 2 || sent[0] != 0x80|opPong {
		t.Fatalf("expected a pong, got % x", sent)
	}
	if sent[1]&0x80 == 0 {
		t.Error("client frame must set the mask bit")
	}

	// Close ends the stream cleanly rather than looking like an error.
	c = newWSConn(&rwc{r: bytes.NewReader(serverFrame(true, opClose, nil))})
	if _, err := c.readMessage(); err != io.EOF {
		t.Errorf("close gave %v, want io.EOF", err)
	}

	// A 16-bit extended length must be read from the right bytes.
	big := bytes.Repeat([]byte("x"), 300)
	c = newWSConn(&rwc{r: bytes.NewReader(serverFrame(true, opBinary, big))})
	msg, err = c.readMessage()
	if err != nil || len(msg) != 300 {
		t.Errorf("extended length: len=%d err=%v", len(msg), err)
	}
}

func TestWSRejectsMalformed(t *testing.T) {
	// A reserved bit means an extension we never negotiated; reading on would
	// misinterpret the payload rather than fail loudly.
	bad := serverFrame(true, opBinary, []byte("x"))
	bad[0] |= 0x40
	if _, err := newWSConn(&rwc{r: bytes.NewReader(bad)}).readMessage(); err == nil {
		t.Error("expected reserved bits to be rejected")
	}

	// Control frames may not be fragmented.
	if _, err := newWSConn(&rwc{r: bytes.NewReader(serverFrame(false, opPing, nil))}).readMessage(); err == nil {
		t.Error("expected fragmented control frame to be rejected")
	}

	// A payload shorter than its header claims is a cut-off stream.
	short := serverFrame(true, opBinary, []byte("hello"))[:5]
	if _, err := newWSConn(&rwc{r: bytes.NewReader(short)}).readMessage(); err == nil {
		t.Error("expected truncated payload to be rejected")
	}

	// A continuation with nothing to continue is a protocol error, not an
	// empty message.
	if _, err := newWSConn(&rwc{r: bytes.NewReader(serverFrame(true, opContinuation, []byte("x")))}).readMessage(); err == nil {
		t.Error("expected orphan continuation to be rejected")
	}
}

func TestExitCodeFromStatus(t *testing.T) {
	// The exec stream reports success and failure the same way — through a
	// Status on the error channel — so this is the only source of exit codes.
	cases := []struct {
		name    string
		payload string
		want    int
		ok      bool
	}{
		{"success", `{"status":"Success"}`, 0, true},
		{"non-zero exit", `{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"2"}]}}`, 2, true},
		{"failure without a code still fails", `{"status":"Failure","message":"boom"}`, 1, true},
		{"not json", `not json at all`, 0, false},
		{"empty", ``, 0, false},
	}
	for _, tc := range cases {
		got, ok := exitCodeFromStatus([]byte(tc.payload))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestShellQuote(t *testing.T) {
	// Directory names reach a shell in the k8s exec path, so a quote in one
	// must not be able to end the argument.
	for in, want := range map[string]string{
		"/workspace":       `'/workspace'`,
		"/workspace/a b":   `'/workspace/a b'`,
		`/workspace/it's`:  `'/workspace/it'\''s'`,
		`/w'; rm -rf /; '`: `'/w'\''; rm -rf /; '\'''`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
