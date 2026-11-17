package fakegoogle

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	chat "google.golang.org/api/chat/v1"
)

func TestMembershipModel(t *testing.T) {
	s := New(t, nil)
	name := "spaces/room/members/123456789"
	s.Configure(func(s *Server) { s.Memberships[name] = &chat.Membership{Name: name, State: "INVITED"} })
	for _, step := range []struct{ method, path, body, state string }{
		{"GET", name, "", "INVITED"}, // ASSUMED U2; G8 documents the resource shape.
		{"POST", "spaces/room/members", `{"member":{"name":"users/123456789","type":"HUMAN"}}`, "JOINED"}, // ASSUMED U1; G3/G4 document the create.
		{"GET", name, "", "JOINED"}, // DOCUMENTED G1: a joined membership's state.
	} {
		req, _ := http.NewRequest(step.method, s.URL()+"/v1/"+step.path, strings.NewReader(step.body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m chat.Membership
		err = json.NewDecoder(res.Body).Decode(&m)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || m.State != step.state {
			t.Fatalf("%s: status=%d state=%s error=%v", step.method, res.StatusCode, m.State, err)
		}
	}
}

func TestMembershipCatchupAndJoinShareModel(t *testing.T) {
	s := New(t, nil)
	name := "spaces/room/members/123456789"
	other := "spaces/other/members/123456789"
	s.Configure(func(s *Server) {
		s.Memberships[name] = &chat.Membership{Name: name, State: "INVITED"}
		s.MembershipErrors[name] = http.StatusTooManyRequests
		s.MembershipReadError = &MembershipError{HTTP: http.StatusServiceUnavailable, Status: "UNAVAILABLE"}
	})
	request := func(method, path, body string, status int, state, errorStatus string) {
		t.Helper()
		req, err := http.NewRequest(method, s.URL()+"/v1/"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var answer struct {
			State string `json:"state"`
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		if err := json.NewDecoder(res.Body).Decode(&answer); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != status || answer.State != state || answer.Error.Status != errorStatus {
			t.Fatalf("%s %s: status=%d state=%s error=%s", method, path, res.StatusCode, answer.State, answer.Error.Status)
		}
	}
	// A per-path failure wins over the global read failure; both are recorded.
	request("GET", name, "", 429, "", "UNKNOWN")
	request("GET", other, "", 503, "", "UNAVAILABLE")
	s.Configure(func(s *Server) {
		delete(s.MembershipErrors, name)
		s.MembershipReadError = nil
	})
	request("GET", name, "", 200, "INVITED", "")
	request("POST", "spaces/room/members", `{"member":{"name":"users/123456789","type":"HUMAN"}}`, 200, "JOINED", "")
	request("GET", name, "", 200, "JOINED", "")
	s.Configure(func(s *Server) {
		if want := []string{name, other, name, name}; !reflect.DeepEqual(s.MembershipRequests, want) {
			t.Errorf("membership reads = %v, want %v", s.MembershipRequests, want)
		}
		want := []MembershipCall{
			{Method: "GET", Path: name},
			{Method: "GET", Path: other},
			{Method: "GET", Path: name},
			{Method: "POST", Path: "spaces/room/members", Member: UserID},
			{Method: "GET", Path: name},
		}
		if !reflect.DeepEqual(s.MembershipCalls, want) {
			t.Errorf("membership calls = %+v, want %+v", s.MembershipCalls, want)
		}
	})
}
