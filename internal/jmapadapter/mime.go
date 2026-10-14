package jmapadapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	jmail "github.com/cr1cr1/o365bridge/jmap/mail"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type parsedPart struct {
	Data []byte
	Type string
	Name string
}

type mimeInfo struct {
	Subject    string
	MessageID  []string
	InReplyTo  []string
	References []string
	Sender     *jmail.EmailAddress
	From       []jmail.EmailAddress
	To         []jmail.EmailAddress
	Cc         []jmail.EmailAddress
	Bcc        []jmail.EmailAddress
	ReplyTo    []jmail.EmailAddress
	SentAt     *time.Time

	TextBody    []jmail.BodyPart
	HTMLBody    []jmail.BodyPart
	Attachments []jmail.BodyPart
	BodyValues  map[string]*jmail.BodyValue
	Parts       map[string]parsedPart
	Text        string
}

var wordDecoder = &mime.WordDecoder{}

func decodeWord(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	out, err := wordDecoder.Decode(s)
	if err != nil {
		return s
	}
	return out
}

func addrList(h stdmail.Header, key string) []jmail.EmailAddress {
	addrs, err := h.AddressList(key)
	if err != nil || len(addrs) == 0 {
		raw := strings.TrimSpace(h.Get(key))
		if raw == "" {
			return nil
		}
		if a, err2 := stdmail.ParseAddress(raw); err2 == nil {
			return []jmail.EmailAddress{{Name: decodeWord(a.Name), Email: a.Address}}
		}
		return []jmail.EmailAddress{{Email: raw}}
	}
	out := make([]jmail.EmailAddress, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, jmail.EmailAddress{Name: decodeWord(a.Name), Email: a.Address})
	}
	return out
}

func decodeTransfer(r io.Reader, enc string) []byte {
	var data []byte
	var err error
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		data, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, r))
	case "quoted-printable":
		data, err = io.ReadAll(quotedprintable.NewReader(r))
	default:
		data, err = io.ReadAll(r)
	}
	if err != nil {
		return nil
	}
	return data
}

// parseMIME parses raw MIME. emailID is used for attachment part blob IDs ("" => nil blob).
// Parsing MIME on every get is the live-translator cost — acceptable, no cache (stateless).
func parseMIME(raw []byte, emailID jmap.ID) (*mimeInfo, error) {
	msg, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	info := &mimeInfo{BodyValues: map[string]*jmail.BodyValue{}, Parts: map[string]parsedPart{}}
	h := msg.Header
	info.Subject = decodeWord(h.Get("Subject"))
	if v := strings.TrimSpace(h.Get("Message-Id")); v != "" {
		info.MessageID = []string{v}
	}
	if v := strings.TrimSpace(h.Get("In-Reply-To")); v != "" {
		info.InReplyTo = []string{v}
	}
	if v := strings.TrimSpace(h.Get("References")); v != "" {
		info.References = strings.Fields(v)
	}
	info.From = addrList(h, "From")
	info.To = addrList(h, "To")
	info.Cc = addrList(h, "Cc")
	info.Bcc = addrList(h, "Bcc")
	info.ReplyTo = addrList(h, "Reply-To")
	if s := addrList(h, "Sender"); len(s) > 0 {
		cp := s[0]
		info.Sender = &cp
	}
	if d, err := h.Date(); err == nil && !d.IsZero() {
		cp := d
		info.SentAt = &cp
	}
	counts := map[string]int{}
	var walk func(r io.Reader, hdr textproto.MIMEHeader)
	walk = func(r io.Reader, hdr textproto.MIMEHeader) {
		ct := hdr.Get("Content-Type")
		if ct == "" {
			ct = "text/plain"
		}
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			mt = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
			if mt == "" {
				mt = "text/plain"
			}
		}
		if strings.HasPrefix(mt, "multipart/") {
			boundary := params["boundary"]
			if boundary == "" {
				return
			}
			mr := multipart.NewReader(r, boundary)
			for {
				p, err := mr.NextPart()
				if err != nil {
					return
				}
				walk(p, p.Header)
			}
		}
		data := decodeTransfer(r, hdr.Get("Content-Transfer-Encoding"))
		disp, dispParams, _ := mime.ParseMediaType(hdr.Get("Content-Disposition"))
		name := params["name"]
		if n := dispParams["filename"]; n != "" {
			name = n
		}
		name = decodeWord(name)
		var partID string
		var isText, isHTML bool
		switch {
		case mt == "text/plain" && disp != "attachment":
			counts["t"]++
			partID = fmt.Sprintf("t%d", counts["t"])
			isText = true
		case mt == "text/html" && disp != "attachment":
			counts["h"]++
			partID = fmt.Sprintf("h%d", counts["h"])
			isHTML = true
		default:
			counts["a"]++
			partID = fmt.Sprintf("a%d", counts["a"])
		}
		info.Parts[partID] = parsedPart{Data: data, Type: mt, Name: name}
		var blob *jmap.ID
		if emailID != "" && !isText && !isHTML {
			b := partBlobID(emailID, partID)
			blob = &b
		}
		part := jmail.BodyPart{PartID: partID, BlobID: blob, Size: uint(len(data)), Type: mt}
		if name != "" {
			part.Name = &name
		}
		if cs := params["charset"]; cs != "" {
			part.Charset = &cs
		}
		if disp != "" {
			part.Disposition = &disp
		}
		switch {
		case isText:
			info.TextBody = append(info.TextBody, part)
			val := string(data)
			info.BodyValues[partID] = &jmail.BodyValue{Value: val}
			if counts["t"] == 1 {
				info.Text = val
			}
		case isHTML:
			info.HTMLBody = append(info.HTMLBody, part)
			info.BodyValues[partID] = &jmail.BodyValue{Value: string(data)}
		default:
			info.Attachments = append(info.Attachments, part)
		}
	}
	walk(msg.Body, textproto.MIMEHeader(h))
	if info.TextBody == nil {
		info.TextBody = []jmail.BodyPart{}
	}
	if info.HTMLBody == nil {
		info.HTMLBody = []jmail.BodyPart{}
	}
	if info.Attachments == nil {
		info.Attachments = []jmail.BodyPart{}
	}
	return info, nil
}

func threadIDFor(info *mimeInfo, upstreamID string) jmap.ID {
	root := ""
	if len(info.References) > 0 {
		root = info.References[0]
	} else if len(info.MessageID) > 0 {
		root = info.MessageID[0]
	} else {
		root = upstreamID
	}
	sum := sha256.Sum256([]byte(root))
	return jmap.ID(base64.RawURLEncoding.EncodeToString(sum[:16]))
}

func previewOf(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	r := []rune(collapsed)
	if len(r) > 100 {
		r = r[:100]
	}
	return string(r)
}

func emailHash(meta bridge.MessageMeta) string {
	flags := append([]string(nil), meta.Flags...)
	sort.Strings(flags)
	h := sha256.Sum256([]byte(strings.Join([]string{
		meta.Subject, meta.From, meta.Date.UTC().Format(time.RFC3339),
		strings.Join(flags, ","), fmt.Sprint(meta.Size), meta.FolderID,
	}, "|")))
	return fmt.Sprintf("%x", h)
}

func buildEmail(meta bridge.MessageMeta, raw []byte) jmail.Email {
	id := encodeEmailID(meta.FolderID, meta.ID)
	info, err := parseMIME(raw, id)
	if err != nil {
		return jmail.Email{
			ID: id, BlobID: id, MailboxIDs: map[jmap.ID]bool{jmap.ID(meta.FolderID): true},
			Keywords: flagsToKeywords(meta.Flags), Size: uint(sizeOf(meta, raw)),
			ReceivedAt: meta.Date, Subject: meta.Subject, HasAttachment: meta.Attachments,
			BodyValues: map[string]*jmail.BodyValue{}, TextBody: []jmail.BodyPart{}, HTMLBody: []jmail.BodyPart{}, Attachments: []jmail.BodyPart{},
			ThreadID: threadIDFor(&mimeInfo{}, meta.ID),
		}
	}
	subject := meta.Subject
	if subject == "" {
		subject = info.Subject
	}
	from := info.From
	if len(from) == 0 && meta.From != "" {
		from = []jmail.EmailAddress{{Email: meta.From}}
	}
	return jmail.Email{
		ID: id, BlobID: id, ThreadID: threadIDFor(info, meta.ID),
		MailboxIDs: map[jmap.ID]bool{jmap.ID(meta.FolderID): true},
		Keywords:   flagsToKeywords(meta.Flags), Size: uint(sizeOf(meta, raw)),
		ReceivedAt: meta.Date, MessageID: info.MessageID, InReplyTo: info.InReplyTo, References: info.References,
		Sender: info.Sender, From: from, To: info.To, Cc: info.Cc, Bcc: info.Bcc, ReplyTo: info.ReplyTo,
		Subject: subject, SentAt: info.SentAt,
		HasAttachment: meta.Attachments || len(info.Attachments) > 0,
		Preview:       previewOf(info.Text),
		BodyValues:    info.BodyValues, TextBody: info.TextBody, HTMLBody: info.HTMLBody, Attachments: info.Attachments,
	}
}

func sizeOf(meta bridge.MessageMeta, raw []byte) int64 {
	if meta.Size != 0 {
		return meta.Size
	}
	return int64(len(raw))
}

func buildParsed(raw []byte, blobID jmap.ID) (jmail.Email, error) {
	info, err := parseMIME(raw, "")
	if err != nil {
		return jmail.Email{}, err
	}
	return jmail.Email{
		BlobID: blobID, ThreadID: threadIDFor(info, string(blobID)),
		MailboxIDs: map[jmap.ID]bool{}, Keywords: map[string]bool{},
		Size: uint(len(raw)), MessageID: info.MessageID, InReplyTo: info.InReplyTo, References: info.References,
		Sender: info.Sender, From: info.From, To: info.To, Cc: info.Cc, Bcc: info.Bcc, ReplyTo: info.ReplyTo,
		Subject: info.Subject, SentAt: info.SentAt, HasAttachment: len(info.Attachments) > 0,
		Preview: previewOf(info.Text), BodyValues: info.BodyValues,
		TextBody: info.TextBody, HTMLBody: info.HTMLBody, Attachments: info.Attachments,
	}, nil
}
