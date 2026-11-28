package owners

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"time"
)

// Answer is the door's word. Result is "" when the door did not answer in
// DoorWait, could not be reached, or answered outside its line form.
type Answer struct {
	Result string
	Claim  string
}

// doorLine is at most 512 bytes with its newline, in and out.
const doorLine = 512

// The answers of each request, and whether each carries a claim id.
var (
	takeAnswers   = map[string]bool{"taken": true, "already_owner": true, "wrong": true, "tries_ended": true, "full": true, "not_live": false, "refused": false, "unavailable": false}
	removeAnswers = map[string]bool{"removed": false, "not_owner": false, "refused": false, "unavailable": false}
	hereAnswers   = map[string]bool{"here": false, "refused": false, "unavailable": false}
)

// Take is the door's first request: the 12 characters, the sender's Chat id
// and the message's name. The door decides; the adapter only tells.
func (b *Book) Take(ctx context.Context, code, sender, message string) Answer {
	if !validCode(code) || !ChatUserID.MatchString(sender) || !ValidMessage(message) {
		return Answer{}
	}
	return b.call(ctx, map[string]string{"request": "take", "code": code, "sender": sender, "message": message}, takeAnswers)
}

// Remove is the door's second request: the Chat id to remove and the name
// of the owner's message that said yes.
func (b *Book) Remove(ctx context.Context, owner, message string) Answer {
	if !ChatUserID.MatchString(owner) || !ValidMessage(message) {
		return Answer{}
	}
	return b.call(ctx, map[string]string{"request": "remove", "owner": owner, "message": message}, removeAnswers)
}

// Here is the door's third request. It is true only when the door answers here.
func (b *Book) Here(ctx context.Context) bool {
	return b.call(ctx, map[string]string{"request": "here"}, hereAnswers).Result == "here"
}

func (b *Book) call(ctx context.Context, request map[string]string, answers map[string]bool) Answer {
	path, ok := trustedSocket(b.r.dir, b.r.isRoot)
	if !ok {
		return Answer{}
	}
	ctx, cancel := context.WithTimeout(ctx, b.r.wait)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Answer{}
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if conn.SetDeadline(deadline) != nil {
		return Answer{}
	}
	// A caller's cancellation ends the call at once, inside the bound.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	line, err := json.Marshal(request)
	if err != nil || len(line)+1 > doorLine {
		return Answer{}
	}
	if _, err = conn.Write(append(line, '\n')); err != nil {
		return Answer{}
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, doorLine)).ReadSlice('\n')
	if err != nil {
		return Answer{}
	}
	fields, err := object(reply[:len(reply)-1])
	if err != nil {
		return Answer{}
	}
	var a Answer
	if fields["result"] == nil || json.Unmarshal(fields["result"], &a.Result) != nil {
		return Answer{}
	}
	claim, known := answers[a.Result]
	switch {
	case !known:
		return Answer{}
	case claim && (len(fields) != 2 || fields["claim"] == nil || json.Unmarshal(fields["claim"], &a.Claim) != nil || !claimID.MatchString(a.Claim)):
		return Answer{}
	case !claim && len(fields) != 1:
		return Answer{}
	}
	return a
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func validCode(code string) bool {
	if len(code) != 12 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !isCodeChar(code[i]) {
			return false
		}
	}
	return true
}

func isCodeChar(c byte) bool {
	for i := 0; i < len(codeAlphabet); i++ {
		if codeAlphabet[i] == c {
			return true
		}
	}
	return false
}

// CodeShaped reads a message's whole text as an owner code: spaces and
// hyphens removed and ASCII letters made upper case, exactly 12 characters of
// the code's alphabet. Any other character, a tab or a newline included,
// leaves the text as it is: not code-shaped.
func CodeShaped(text string) (string, bool) {
	out := make([]byte, 0, 12)
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == ' ' || c == '-':
			continue
		case 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
		}
		if !isCodeChar(c) || len(out) == 12 {
			return "", false
		}
		out = append(out, c)
	}
	if len(out) != 12 {
		return "", false
	}
	return string(out), true
}
