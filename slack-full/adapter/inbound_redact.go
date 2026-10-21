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
	changed := false
	var walk func(any, string) any
	walk = func(v any, context string) any {
		switch v := v.(type) {
		case string:
			clean := redactInboundText(v)
			changed = changed || clean != v
			return clean
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
	if !changed {
		return raw, nil
	} // preserve frozen bytes/digests for ordinary content
	return json.Marshal(value)
}

func redactInboundMessage(msg externalInboundMessage) externalInboundMessage {
	msg.Text = redactAttachmentText(msg.Text, msg)
	msg.Actor.DisplayName = redactInboundText(msg.Actor.DisplayName)
	return msg
}

// Keep only the adapter's structured local attachment paths usable in its
// generated prose. Absolute paths can themselves look like base64. Incoming
// Slack text/file names were already redacted before these paths were built.
func redactAttachmentText(text string, msg externalInboundMessage) string {
	urls := append([]string(nil), msg.trustedFilePaths...)
	for _, att := range msg.Attachments {
		urls = append(urls, att.URL)
	}
	var paths []string
	for _, url := range urls {
		if strings.HasPrefix(url, "file:///") {
			paths = append(paths, url, strings.TrimPrefix(url, "file://"))
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
	if len(e.Inbound.trustedFilePaths) == 0 {
		e.Inbound.trustedFilePaths = e.TrustedFilePaths
	}
	e.TrustedFilePaths = append([]string(nil), e.Inbound.trustedFilePaths...)
	e.Inbound = redactInboundMessage(e.Inbound)
	e.ThreadAnchor = redactInboundText(e.ThreadAnchor)
	e.Preamble = redactInboundText(e.Preamble)
	e.PreambleLean = redactInboundText(e.PreambleLean)
	e.Body = redactAttachmentText(e.Body, e.Inbound)
	e.Files = redactAttachmentText(e.Files, e.Inbound)
	e.Refused = redactInboundText(e.Refused)
	e.Stripped = redactInboundText(e.Stripped)
	e.Verdict = redactInboundText(e.Verdict)
	return e
}

// Old frozen hydration is scrubbed at rendering as well as before a receipt
// update. The JSON round trip copies its nested pointers/slices and applies
// the same content-field policy without changing routing/turn identifiers.
func redactCompanyHydration(h companyHydration) companyHydration {
	raw, err := json.Marshal(h)
	if err != nil {
		return companyHydration{}
	}
	raw, err = redactEventContent(raw)
	if err != nil {
		return companyHydration{}
	}
	var clean companyHydration
	_ = json.Unmarshal(raw, &clean)
	return clean
}
