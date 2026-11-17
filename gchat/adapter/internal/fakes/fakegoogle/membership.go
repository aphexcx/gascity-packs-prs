package fakegoogle

import (
	"net/http"
	"strings"

	chat "google.golang.org/api/chat/v1"
)

type MembershipError struct {
	HTTP   int
	Status string
	Detail string
}

type MembershipCall struct {
	Method string
	Path   string
	Member string
}

// membership deliberately supports only self reads and self creates. The
// invitation transitions are ASSUMED (U1-U4), pending a real proof call.
// The resource shapes and HUMAN member type are DOCUMENTED (G3, G4, G8).
func (s *Server) membership(w http.ResponseWriter, r *http.Request, path string) {
	call := MembershipCall{Method: r.Method, Path: path}
	defer func() { s.MembershipCalls = append(s.MembershipCalls, call) }()
	parts := strings.Split(path, "/")
	if r.Method == http.MethodGet && len(parts) == 4 {
		s.MembershipRequests = append(s.MembershipRequests, path)
		if code := s.MembershipErrors[path]; code != 0 {
			s.fail(w, code, "UNKNOWN", "scripted membership failure")
			return
		}
		if e := s.MembershipReadError; e != nil {
			s.fail(w, e.HTTP, e.Status, e.Detail)
			return
		}
		if member := s.Memberships[path]; member != nil {
			s.respond(w, r, 200, member)
		} else {
			s.fail(w, 404, "NOT_FOUND", "no invitation")
		}
		return
	}
	if r.Method != http.MethodPost || len(parts) != 3 {
		s.fail(w, 405, "INVALID_ARGUMENT", "only membership reads and self creates are modeled")
		return
	}
	var member chat.Membership
	if !s.decode(w, r, &member) {
		return
	}
	if member.Member != nil {
		call.Member = member.Member.Name
	}
	if member.Member == nil || member.Member.Name != UserID || member.Member.Type != "HUMAN" || member.State != "" {
		s.fail(w, 400, "INVALID_ARGUMENT", "only the identity herself may join; state is output only")
		return
	}
	if e := s.MembershipWriteError; e != nil {
		s.fail(w, e.HTTP, e.Status, e.Detail)
		return
	}
	name := path + "/" + strings.TrimPrefix(UserID, "users/")
	previous := s.Memberships[name]
	if previous == nil || previous.State == "NOT_A_MEMBER" {
		s.fail(w, 404, "NOT_FOUND", "no invitation")
		return
	}
	member.Name, member.State = name, "JOINED"
	if s.MembershipCreateState != "" {
		member.State = s.MembershipCreateState
	}
	s.Memberships[name] = &member
	s.respond(w, r, 200, &member)
}
