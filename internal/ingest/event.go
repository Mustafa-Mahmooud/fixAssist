package ingest

import (
	"encoding/json"
	"fmt"
)

// Event is the subset of the Sentry event payload we use.
// Unknown fields are ignored; the raw payload is kept by the caller.
type Event struct {
	EventID     string                     `json:"event_id"`
	Level       string                     `json:"level"`
	Platform    string                     `json:"platform"`
	Release     string                     `json:"release"`
	Environment string                     `json:"environment"`
	Transaction string                     `json:"transaction"`
	Exception   *ExceptionList             `json:"exception"`
	Request     *Request                   `json:"request"`
	Tags        map[string]string          `json:"-"`
	Contexts    map[string]json.RawMessage `json:"contexts"`
	SDK         *SDKInfo                   `json:"sdk"`
	Message     json.RawMessage            `json:"message"` // string or {"formatted": ...}
	Logentry    *struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	} `json:"logentry"`
}

// MessageText is the event's plain message, for events without exceptions.
func (e *Event) MessageText() string {
	if e.Logentry != nil {
		if e.Logentry.Formatted != "" {
			return e.Logentry.Formatted
		}
		return e.Logentry.Message
	}
	if len(e.Message) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(e.Message, &s) == nil {
		return s
	}
	var obj struct{ Formatted, Message string }
	if json.Unmarshal(e.Message, &obj) == nil {
		if obj.Formatted != "" {
			return obj.Formatted
		}
		return obj.Message
	}
	return ""
}

// ExceptionList wraps exception.values.
type ExceptionList struct {
	Values []Exception `json:"values"`
}

// Exception is one exception in the chain (the last is the outermost).
type Exception struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Module     string      `json:"module"`
	Stacktrace *Stacktrace `json:"stacktrace"`
	Mechanism  *struct {
		Type    string `json:"type"`
		Handled *bool  `json:"handled"`
	} `json:"mechanism"`
}

// Stacktrace frames are ordered oldest call first, crash site last.
type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

// Frame is one stack frame.
type Frame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"abs_path"`
	Function string `json:"function"`
	Module   string `json:"module"`
	Lineno   int    `json:"lineno"`
	Colno    int    `json:"colno"`
	InApp    *bool  `json:"in_app"`
	// Server SDKs send the source line; browser SDKs only with source maps.
	ContextLine string `json:"context_line"`
}

// IsServer reports whether the event came from a server SDK (any language)
// rather than the browser. Browser SDKs send platform "javascript"; server
// SDKs send their language: node, python, csharp, php, ruby, java, go, ...
func (e *Event) IsServer() bool {
	return e.Platform != "" && e.Platform != "javascript"
}

// Request is the page the event happened on.
type Request struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// HTTPContext is what the fix add-on puts in contexts.fix_http for
// failed fetch/XHR calls (event exception type "HTTPError").
type HTTPContext struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

// ParseEvent decodes an event payload. Tags may be an object or a list of
// [key, value] pairs, so they are decoded separately.
func ParseEvent(payload []byte) (*Event, error) {
	var ev Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	var raw struct {
		Tags json.RawMessage `json:"tags"`
	}
	_ = json.Unmarshal(payload, &raw)
	ev.Tags = decodeTags(raw.Tags)
	return &ev, nil
}

func decodeTags(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			out[k] = fmt.Sprint(v)
		}
		return out
	}
	var pairs [][]any
	if json.Unmarshal(raw, &pairs) == nil {
		for _, p := range pairs {
			if len(p) == 2 {
				out[fmt.Sprint(p[0])] = fmt.Sprint(p[1])
			}
		}
	}
	return out
}

// MainException returns the outermost exception, or nil.
func (e *Event) MainException() *Exception {
	if e.Exception == nil || len(e.Exception.Values) == 0 {
		return nil
	}
	return &e.Exception.Values[len(e.Exception.Values)-1]
}

// HTTP returns the add-on's network context, if this is an HTTPError event.
func (e *Event) HTTP() (*HTTPContext, bool) {
	raw, ok := e.Contexts["fix_http"]
	if !ok {
		return nil, false
	}
	var h HTTPContext
	if json.Unmarshal(raw, &h) != nil {
		return nil, false
	}
	return &h, true
}

// PageURL is the URL of the page the event happened on.
func (e *Event) PageURL() string {
	if e.Request == nil {
		return ""
	}
	return e.Request.URL
}
