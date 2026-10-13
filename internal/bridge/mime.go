package bridge

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"time"
)

// FactoryFunc adapts a function to Factory.
type FactoryFunc func(email string, s Session) Provider

// New implements Factory.
func (f FactoryFunc) New(email string, s Session) Provider { return f(email, s) }

// ParsedMessage is the provider-neutral view of a raw MIME message,
// used by upstream providers that cannot send raw MIME directly.
type ParsedMessage struct {
	Subject   string
	From      string
	To        []string
	Cc        []string
	Date      time.Time
	MessageID string
	Text      string
	HTML      string
}

// ParseMIME extracts envelope and body parts from a raw RFC 5322
// message. Attachments are not decoded; providers that send via a
// JSON API use Text/HTML, providers with a MIME endpoint should use
// the raw bytes instead of this view.
func ParseMIME(raw []byte) (ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("bridge: parse mime: %w", err)
	}
	var out ParsedMessage
	out.Subject = decodeHeader(msg.Header.Get("Subject"))
	out.From = msg.Header.Get("From")
	out.MessageID = msg.Header.Get("Message-Id")
	if t, err := msg.Header.Date(); err == nil {
		out.Date = t
	}
	for _, h := range []struct {
		name string
		dst  *[]string
	}{
		{"To", &out.To}, {"Cc", &out.Cc},
	} {
		if v := msg.Header.Get(h.name); v != "" {
			if addrs, err := msg.Header.AddressList(h.name); err == nil {
				for _, a := range addrs {
					*h.dst = append(*h.dst, a.Address)
				}
			} else {
				*h.dst = append(*h.dst, v)
			}
		}
	}
	ct := msg.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out, fmt.Errorf("bridge: parse mime part: %w", err)
			}
			sub, err := parsePart(part)
			if err != nil {
				return out, err
			}
			if out.Text == "" {
				out.Text = sub.Text
			}
			if out.HTML == "" {
				out.HTML = sub.HTML
			}
		}
		return out, nil
	}
	sub, err := parsePartBody(msg.Body, mt)
	if err != nil {
		return out, err
	}
	out.Text, out.HTML = sub.Text, sub.HTML
	return out, nil
}

type bodyParts struct{ Text, HTML string }

func parsePart(part *multipart.Part) (bodyParts, error) {
	ct := part.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err == nil && strings.HasPrefix(mt, "multipart/") {
		// Nested multiparts (e.g. alternative inside related): walk.
		mr := multipart.NewReader(part, params["boundary"])
		var agg bodyParts
		for {
			sub, err := mr.NextPart()
			if err == io.EOF {
				return agg, nil
			}
			if err != nil {
				return agg, err
			}
			got, err := parsePart(sub)
			if err != nil {
				return agg, err
			}
			if agg.Text == "" {
				agg.Text = got.Text
			}
			if agg.HTML == "" {
				agg.HTML = got.HTML
			}
		}
	}
	return parsePartBody(part, mt)
}

func parsePartBody(r io.Reader, mt string) (bodyParts, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return bodyParts{}, err
	}
	switch mt {
	case "text/html":
		return bodyParts{HTML: string(data)}, nil
	case "text/plain", "":
		return bodyParts{Text: string(data)}, nil
	default:
		return bodyParts{}, nil // attachments etc. are skipped in this view
	}
}

func decodeHeader(v string) string {
	dec := new(mime.WordDecoder)
	out, err := dec.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}
