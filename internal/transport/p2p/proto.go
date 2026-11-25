package p2p

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hauntedfail/geminus/internal/honcho"
)

const (
	alpnProto     = "geminise-v2"
	maxFrameBytes = 16 << 20 // 16 MiB
)

type rpcRequest struct {
	Method   string          `json:"method"`
	Peer     string          `json:"peer,omitempty"`
	Key      string          `json:"key,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Filter   map[string]any  `json:"filter,omitempty"`
	Session  string          `json:"session,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Peers    []string        `json:"peers,omitempty"`
	Limit    int             `json:"limit,omitempty"`
	Content  string          `json:"content,omitempty"`
	Endpoint string          `json:"endpoint,omitempty"`
}

type rpcResponse struct {
	Error    string           `json:"error,omitempty"`
	Code     string           `json:"code,omitempty"`
	Peers    []honcho.Peer    `json:"peers,omitempty"`
	Sessions []honcho.Session `json:"sessions,omitempty"`
	Messages []honcho.Message `json:"messages,omitempty"`
	Message  *honcho.Message  `json:"message,omitempty"`
}

func writeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxFrameBytes {
		return fmt.Errorf("p2p: frame exceeds %d bytes", maxFrameBytes)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func readFrame(r io.Reader, out any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrameBytes {
		return errors.New("p2p: invalid frame length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, out)
}
