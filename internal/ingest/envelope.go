// Package ingest parses Sentry envelopes and legacy store payloads.
// It only parses, authenticates and hands events to a Sink; it never makes
// slow calls (see FDR "Request path").
package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxEnvelopeBytes caps a decompressed envelope (FDR ingest step 3).
const MaxEnvelopeBytes = 1 << 20

// Envelope is a parsed Sentry envelope: one header line followed by items.
type Envelope struct {
	Header EnvelopeHeader
	Items  []Item
}

// EnvelopeHeader is the first line of an envelope.
type EnvelopeHeader struct {
	EventID string          `json:"event_id,omitempty"`
	DSN     string          `json:"dsn,omitempty"`
	SentAt  string          `json:"sent_at,omitempty"`
	SDK     *SDKInfo        `json:"sdk,omitempty"`
	Trace   json.RawMessage `json:"trace,omitempty"`
}

// SDKInfo identifies the sending SDK.
type SDKInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ItemHeader precedes each item payload.
type ItemHeader struct {
	Type        string `json:"type"`
	Length      *int   `json:"length,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// Item is one envelope item. Payload is the raw item body.
type Item struct {
	Header  ItemHeader
	Payload []byte
}

// Item types we accept as events; everything else is acked and dropped.
const (
	ItemEvent = "event"
)

// ErrTooLarge is returned when an envelope exceeds MaxEnvelopeBytes.
var ErrTooLarge = errors.New("envelope too large")

// ParseEnvelope parses the newline-delimited envelope format:
//
//	{envelope header}\n
//	{item header}\n
//	{payload}\n   (exactly "length" bytes when length is set, else up to the newline)
//	...
func ParseEnvelope(data []byte) (*Envelope, error) {
	if len(data) > MaxEnvelopeBytes {
		return nil, ErrTooLarge
	}
	r := bufio.NewReaderSize(bytes.NewReader(data), 64*1024)

	line, err := readLine(r)
	if err != nil {
		return nil, fmt.Errorf("envelope header: %w", err)
	}
	env := &Envelope{}
	if len(bytes.TrimSpace(line)) > 0 {
		if err := json.Unmarshal(line, &env.Header); err != nil {
			return nil, fmt.Errorf("envelope header: %w", err)
		}
	}

	for {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			continue // tolerate blank lines between items
		}
		var ih ItemHeader
		if jerr := json.Unmarshal(line, &ih); jerr != nil {
			return nil, fmt.Errorf("item %d header: %w", len(env.Items), jerr)
		}

		var payload []byte
		if ih.Length != nil {
			n := *ih.Length
			if n < 0 || n > MaxEnvelopeBytes {
				return nil, fmt.Errorf("item %d: bad length %d", len(env.Items), n)
			}
			payload = make([]byte, n)
			if _, rerr := io.ReadFull(r, payload); rerr != nil {
				return nil, fmt.Errorf("item %d payload: %w", len(env.Items), rerr)
			}
			// Consume the optional trailing newline.
			if b, perr := r.Peek(1); perr == nil && b[0] == '\n' {
				_, _ = r.ReadByte()
			}
		} else {
			p, perr := readLine(r)
			if perr != nil && !errors.Is(perr, io.EOF) {
				return nil, perr
			}
			payload = p
		}
		env.Items = append(env.Items, Item{Header: ih, Payload: payload})
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return env, nil
}

// readLine returns one line without its trailing "\n" (or "\r\n").
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if err != nil {
			return buf, err
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Events decodes every "event" item in the envelope. Unknown item types
// (session, client_report, transaction, replay, ...) are skipped.
func (e *Envelope) Events() ([]*Event, error) {
	var out []*Event
	for i, it := range e.Items {
		if it.Header.Type != ItemEvent {
			continue
		}
		ev, err := ParseEvent(it.Payload)
		if err != nil {
			return out, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, ev)
	}
	return out, nil
}
