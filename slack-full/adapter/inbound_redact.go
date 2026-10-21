package main

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/sjarmak/gc-slack-adapter/redact"
)

var redactInboundText = redact.Text

// redactEventContent runs after transport authentication and before any durable
// admission. Walk decoded JSON so Unicode escapes and nested Block Kit text
// cannot bypass it. Ordinary routing IDs/nonces do not match the token shapes;
// file download URLs are explicitly preserved for the authenticated fetch.
func redactEventContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var walk func(any, string) any
	walk = func(v any, context string) any {
		switch v := v.(type) {
		case string:
			return redactInboundText(v)
		case map[string]any:
			for key, child := range v {
				// These fields are consumed as protocol data, not as prose.
				// All other strings (including links, captions, arbitrary
				// attachment fields and metadata comments) are redacted.
				if context == "files" && (key == "url_private" || key == "url_private_download") {
					continue
				}
				v[key] = walk(child, key)
			}
		case []any:
			for i, child := range v {
				v[i] = walk(child, context)
			}
		}
		return v
	}
	value = walk(value, "")
	return json.Marshal(value)
}

func redactInboundMessage(msg externalInboundMessage) externalInboundMessage {
	msg.Text = redactAttachmentText(msg.Text, msg.Attachments)
	msg.Actor.DisplayName = redactInboundText(msg.Actor.DisplayName)
	return msg
}

// Keep only the adapter's structured local attachment paths usable in its
// generated prose. Absolute paths can themselves look like base64. Incoming
// Slack text/file names were already redacted before these paths were built.
func redactAttachmentText(text string, attachments []externalAttachment) string {
	var paths []string
	for _, att := range attachments {
		if strings.HasPrefix(att.URL, "file:///") {
			paths = append(paths, att.URL, strings.TrimPrefix(att.URL, "file://"))
		}
	}
	var out strings.Builder
	for len(text) > 0 {
		start, path := len(text), ""
		for _, candidate := range paths {
			if at := strings.Index(text, candidate); at >= 0 && (at < start || at == start && len(candidate) > len(path)) {
				start, path = at, candidate
			}
		}
		out.WriteString(redactInboundText(text[:start]))
		out.WriteString(path)
		text = text[start+len(path):]
	}
	return out.String()
}

func redactSpooledInbound(e spooledInbound) spooledInbound {
	e.Inbound = redactInboundMessage(e.Inbound)
	e.ThreadAnchor = redactInboundText(e.ThreadAnchor)
	e.Preamble = redactInboundText(e.Preamble)
	e.PreambleLean = redactInboundText(e.PreambleLean)
	e.Body = redactAttachmentText(e.Body, e.Inbound.Attachments)
	e.Files = redactAttachmentText(e.Files, e.Inbound.Attachments)
	e.Refused = redactInboundText(e.Refused)
	e.Stripped = redactInboundText(e.Stripped)
	e.Verdict = redactInboundText(e.Verdict)
	return e
}
