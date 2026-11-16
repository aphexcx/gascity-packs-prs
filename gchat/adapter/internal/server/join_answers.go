package server

import (
	"errors"
	"fmt"

	"gchat-adapter/internal/chat"
)

const joinAdminStep = "This box cannot accept invitations yet. A Workspace super administrator must open Google Admin console > Security > Access and data control > API controls > Manage Domain Wide Delegation, edit this client's entry, add https://www.googleapis.com/auth/chat.memberships beside the existing permissions, and click Authorize; changes can take up to 24 hours."
const joinSoftwareStep = "This box is not set up to hand out that permission yet. Whoever runs the box updates it or turns the permission on; the admin's step alone will not help."
const joinUncertain = "The join outcome is unknown; run it again to read membership before any new write."

var joinMessages = map[string]string{
	"already_member":     "Already a member of this room. Nothing changed.",
	"no_invitation":      "Google shows no invitation for this identity in this room. Check the room id with the person who invited you.",
	"refused_invited":    "An invitation exists, but Google refused the join. A setting of this room's organization or this identity's organization may keep outside people out; an administrator of that organization must allow it.",
	"not_available":      "Not available on this sign-in. This box uses desktop sign-in.",
	"google_no_answer":   "Google did not answer. Nothing changed; run it again.",
	"permission_present": "The permission to accept invitations is present. No room was touched.",
	"requested":          "Ask the owner in a direct message to reply with this room's code before it expires in 24 hours.",
}

// The complete set of modeled API outcomes. State names and meanings are
// DOCUMENTED G1; invitation read/create behavior is ASSUMED U1-U4. Unknown
// HTTP/status pairs are never assigned a more specific cause by body matching.
var joinAnswers = []struct {
	phase                           string
	http                            int
	status, state, result, evidence string
}{
	{"read", 200, "", "JOINED", "already_member", "DOCUMENTED G1/G8"},
	{"read", 200, "", "INVITED", "", "ASSUMED U2; DOCUMENTED G1"},
	{"read", 200, "", "NOT_A_MEMBER", "no_invitation", "DOCUMENTED G1; ASSUMED U3"},
	{"read", 404, "NOT_FOUND", "", "no_invitation", "ASSUMED U3"},
	{"create", 200, "", "JOINED", "joined", "ASSUMED U1; DOCUMENTED G3/G4"},
	{"create", 200, "", "INVITED", "refused_invited", "ASSUMED U1"},
	{"create", 400, "FAILED_PRECONDITION", "", "refused_invited", "ASSUMED U4"},
	{"create", 403, "PERMISSION_DENIED", "", "refused_invited", "ASSUMED U4"},
	{"create", 404, "NOT_FOUND", "", "no_invitation", "ASSUMED U3"},
}

func joinAnswer(phase, state string, err error) joinResult {
	code, status := 200, ""
	if err != nil {
		var e *chat.MembershipError
		if !errors.As(err, &e) {
			if phase == "create" {
				return joinResult{Result: "google_no_answer", Message: "Google did not answer. " + joinUncertain}
			}
			return joinReply("google_no_answer")
		}
		code, status = e.HTTP, e.Status
	}
	for _, row := range joinAnswers {
		if row.phase == phase && row.http == code && row.status == status && row.state == state {
			return joinReply(row.result)
		}
	}
	if phase == "read" && err == nil {
		return joinReply("no_invitation")
	}
	ending := "Nothing changed."
	if phase == "create" {
		ending = joinUncertain
	}
	return joinResult{Result: "google_unknown", Message: fmt.Sprintf("Google answered HTTP %d %s. %s", code, statusWord(status), ending)}
}

func statusWord(s string) string {
	if s == "" {
		return "UNKNOWN"
	}
	return s
}
func joinReply(result string) joinResult {
	return joinResult{Result: result, Message: joinMessages[result]}
}
