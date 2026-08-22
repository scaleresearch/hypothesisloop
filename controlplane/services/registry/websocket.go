package registry

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// A hand-written RFC 6455 server, roughly a hundred lines of it, because the repository vendors
// its dependencies and no WebSocket library is vendored — and `go mod vendor` cannot reach the
// network here to add one. What /watch needs of the protocol is small and fixed: accept the
// handshake, write text frames, answer pings, notice a close. That is all this implements. It is
// server-side only: it never masks what it writes and always unmasks what it reads, per the spec.

const wsGUID = "258EAFA5-E914-47DA-95CA-5AB0DC85B11F"

const (
	wsOpText   = 0x1
	wsOpClose  = 0x8
	wsOpPing   = 0x9
	wsOpPong   = 0xA
	wsFinalBit = 0x80
	wsMaskBit  = 0x80
	// A control frame's payload is capped at 125 bytes by the protocol, and this server has no
	// reason to accept a large data frame from a client that is only ever a reader — see
	// watch.go on why the client half of this socket carries no commands.
	wsMaxClientPayload = 1024
)

// errWSClosed reports an orderly close from the peer, as opposed to a broken connection.
var errWSClosed = errors.New("websocket closed by peer")

type wsConn struct {
	conn net.Conn
	rw   *bufio.ReadWriter
}

// acceptWebSocket completes the RFC 6455 opening handshake and takes the connection off the HTTP
// server, which is why it cannot be a Huma operation: Huma owns the response, and an upgrade
// needs the raw socket. The write deadline the http.Server set before hijacking survives on the
// connection, so it is cleared here — a stream that is meant to stay open for fifteen minutes
// must not inherit a thirty-second write timeout.
func acceptWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerContainsToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("not a websocket upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, fmt.Errorf("unsupported websocket version %q", r.Header.Get("Sec-WebSocket-Version"))
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("connection does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("clear deadline: %w", err)
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // the accept token is specified as SHA-1
	accept := base64.StdEncoding.EncodeToString(sum[:])
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(response); err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("write handshake: %w", err)
	}
	if err := rw.Flush(); err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("flush handshake: %w", err)
	}
	return &wsConn{conn: conn, rw: rw}, nil
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// WriteText sends one text frame. Every write carries its own deadline so a peer that stops
// reading — a suspended container, a dropped route — cannot block the writer forever.
func (c *wsConn) WriteText(payload []byte, deadline time.Duration) error {
	return c.writeFrame(wsOpText, payload, deadline)
}

// WritePing is how a broken connection is discovered on an otherwise idle stream: a job can sit
// RUNNING for an hour with nothing to report, and neither side would otherwise learn that the
// path between them died.
func (c *wsConn) WritePing(deadline time.Duration) error {
	return c.writeFrame(wsOpPing, nil, deadline)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte, deadline time.Duration) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
		return err
	}
	header := []byte{wsFinalBit | opcode}
	switch n := len(payload); {
	case n <= 125:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := c.rw.Write(header); err != nil {
		return err
	}
	if _, err := c.rw.Write(payload); err != nil {
		return err
	}
	return c.rw.Flush()
}

// ReadFrame reads one frame, unmasks it, and returns its opcode. An orderly close comes back as
// errWSClosed so the caller can tell it from a broken pipe.
func (c *wsConn) ReadFrame() (opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.rw, head[:]); err != nil {
		return 0, nil, err
	}
	opcode = head[0] & 0x0F
	masked := head[1]&wsMaskBit != 0
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:])) //nolint:gosec // bounded immediately below
	}
	if length > wsMaxClientPayload {
		return 0, nil, fmt.Errorf("client frame of %d bytes exceeds the %d-byte limit", length, wsMaxClientPayload)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.rw, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if opcode == wsOpClose {
		return opcode, payload, errWSClosed
	}
	return opcode, payload, nil
}

// Close sends a close frame and drops the connection. A best-effort close frame: if the peer is
// already gone there is nothing to report and nothing to retry.
func (c *wsConn) Close() {
	_ = c.writeFrame(wsOpClose, nil, time.Second)
	_ = c.conn.Close()
}
