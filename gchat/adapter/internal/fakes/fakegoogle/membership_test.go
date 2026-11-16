package fakegoogle

import (
	"encoding/json"
	"net/http"
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
