package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ACK_REACTION=model from the hou-dynasty local patch of
// 2026-10-30 and 2026-11-03, upstreamed by gp-n32w.
//
// Same hook point as ACK_REACTION=auto, but the emoji is chosen by a
// small fast model behind an OpenAI-compatible chat completions
// endpoint (the Teale gateway by default). Any failure — timeout,
// transport error, non-2xx, empty or unparseable reply, missing key —
// falls back to the keyword pick (pickAckEmoji), so every message still
// gets exactly one reaction.

const (
	defaultAckModelBaseURL = "https://gateway.teale.com/v1"
	defaultAckModelName    = "teale/auto"
	defaultAckModelTimeout = 6000 * time.Millisecond

	// ackModelMaxTokens is a little above the 8 the spec asked for:
	// "chart_with_upwards_trend" alone can run past 8 tokens, and a
	// truncated name would always fall back.
	ackModelMaxTokens = 16

	// ackModelMaxText bounds the message text sent to the gateway.
	ackModelMaxText = 1000
)

// ackAllowedEmoji is the set the model may choose from, in prompt order.
var ackAllowedEmoji = []string{
	// topic set (same names pickAckEmoji produces)
	"calendar", "house", "wrench", "moneybag", "page_facing_up",
	"chart_with_upwards_trend", "white_check_mark", "pray", "tada",
	"thinking_face", "eyes",
	// extras
	"car", "hammer_and_wrench", "mag", "bulb", "memo", "rocket", "warning",
	"clock1", "speech_balloon", "sunglasses", "muscle", "fire", "heart",
	"ok_hand", "handshake", "globe_with_meridians", "computer", "robot_face",
	"microphone", "camera", "truck", "package", "airplane", "hospital",
	"dog", "cat", "elephant",
}

var ackAllowedSet = func() map[string]bool {
	m := make(map[string]bool, len(ackAllowedEmoji))
	for _, e := range ackAllowedEmoji {
		m[e] = true
	}
	return m
}()

var ackColonNameRE = regexp.MustCompile(`:([a-z0-9_+-]+):`)

// ackModelSystemPrompt tells the model to answer with one emoji name.
var ackModelSystemPrompt = "You pick one emoji reaction that acknowledges a chat message. " +
	"Reply with exactly one emoji name from this list and nothing else (no colons, no explanation): " +
	strings.Join(ackAllowedEmoji, ", ") + "."

// parseAckEmojiReply extracts one allowed emoji name from a model reply.
// A bare name (":tada:", "Tada", " tada \n") is accepted; so is a reply
// containing exactly one allowed :name: token. Anything else — junk,
// empty, a name outside the allowed list, several different names — is
// rejected so the caller falls back.
func parseAckEmojiReply(reply string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(reply))
	if s == "" {
		return "", false
	}
	if bare := strings.Trim(s, ":. \t\r\n"); ackAllowedSet[bare] {
		return bare, true
	}
	found := ""
	for _, m := range ackColonNameRE.FindAllStringSubmatch(s, -1) {
		if !ackAllowedSet[m[1]] {
			continue
		}
		if found != "" && found != m[1] {
			return "", false
		}
		found = m[1]
	}
	return found, found != ""
}

// ackModelUserContent renders the user message sent to the model: the
// message text, attachment names/types, and the channel name. Text is
// secret-redacted and clipped.
func ackModelUserContent(text string, files []slackFile, channel string) string {
	text = redactInboundText(strings.TrimSpace(text))
	if utf8.RuneCountInString(text) > ackModelMaxText {
		text = string([]rune(text)[:ackModelMaxText])
	}
	var b strings.Builder
	if channel != "" {
		fmt.Fprintf(&b, "Channel: #%s\n", channel)
	}
	if len(files) > 0 {
		b.WriteString("Attachments:")
		for _, f := range files {
			name := f.Name
			if name == "" {
				name = f.Title
			}
			kind := f.MIMEType
			if kind == "" {
				kind = f.Filetype
			}
			fmt.Fprintf(&b, " %s (%s);", redactInboundText(name), kind)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Message: %s", text)
	return b.String()
}

type ackChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ackChatRequest struct {
	Model       string           `json:"model"`
	Messages    []ackChatMessage `json:"messages"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens"`
}

type ackChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// askAckModel calls the chat completions endpoint and returns the
// reply content. The key is read from the key file on every call so a
// rotation needs no restart. Errors never include the key or the
// message text.
func askAckModel(cfg config, userContent string) (string, error) {
	if cfg.ackModelKeyFile == "" {
		return "", fmt.Errorf("ACK_MODEL_API_KEY_FILE not set")
	}
	raw, err := os.ReadFile(cfg.ackModelKeyFile)
	if err != nil {
		return "", fmt.Errorf("read api key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("api key file is empty")
	}
	body, err := json.Marshal(ackChatRequest{
		Model: cfg.ackModelName,
		Messages: []ackChatMessage{
			{Role: "system", Content: ackModelSystemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0,
		MaxTokens:   ackModelMaxTokens,
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ackModelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.ackModelBaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// url.Error carries the URL, not the headers or body.
		if ctx.Err() != nil {
			return "", fmt.Errorf("timeout after %s", cfg.ackModelTimeout)
		}
		return "", fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("timeout after %s", cfg.ackModelTimeout)
		}
		return "", fmt.Errorf("read response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var cr ackChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("decode response")
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("empty content")
	}
	return cr.Choices[0].Message.Content, nil
}

// pickAckEmojiModel chooses the emoji for a message with the model,
// falling back to pickAckEmoji on any failure. It always returns a
// usable emoji and logs one line (no message text).
func pickAckEmojiModel(cfg config, text string, files []slackFile, channel string) string {
	start := time.Now()
	emoji, reason := "", ""
	reply, err := askAckModel(cfg, ackModelUserContent(text, files, channel))
	if err != nil {
		reason = err.Error()
	} else if name, ok := parseAckEmojiReply(reply); ok {
		emoji = name
	} else {
		reason = "unparseable reply"
	}
	fallback := emoji == ""
	if fallback {
		emoji = pickAckEmoji(text, len(files) > 0)
	}
	line := fmt.Sprintf("ack reaction model: model=%s latency_ms=%d emoji=%s fallback=%s",
		cfg.ackModelName, time.Since(start).Milliseconds(), emoji, map[bool]string{true: "yes", false: "no"}[fallback])
	if fallback {
		line += " reason=" + fmt.Sprintf("%q", reason)
	}
	log.Print(line)
	return emoji
}
