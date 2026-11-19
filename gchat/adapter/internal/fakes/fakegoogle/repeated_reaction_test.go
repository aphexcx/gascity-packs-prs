package fakegoogle

import (
	"testing"

	chat "google.golang.org/api/chat/v1"
)

// PR 104 round 4: RepeatedReactionConflicts is opt-in. Off, one emoji placed
// three times is three reactions (the off-parity script relies on that); on,
// the second identical create answers 409 ALREADY_EXISTS, another emoji is
// still created, and a deleted reaction can be placed again.
func TestRepeatedReactionConflictsIsOptIn(t *testing.T) {
	for _, on := range []bool{false, true} {
		s := New(t, nil)
		s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
		s.RepeatedReactionConflicts = on
		msg := request(t, s, "POST", "/v1/spaces/room/messages", map[string]string{"text": "hello"}, 200)
		path := "/v1/" + msg["name"].(string) + "/reactions"
		eyes := map[string]any{"emoji": map[string]string{"unicode": "\U0001F440"}}
		first := request(t, s, "POST", path, eyes, 200)
		if !on {
			request(t, s, "POST", path, eyes, 200)
			request(t, s, "POST", path, eyes, 200)
			if list := request(t, s, "GET", path, nil, 200); len(list["reactions"].([]any)) != 3 {
				t.Fatalf("off: reactions %v, want three", list)
			}
			continue
		}
		answer := request(t, s, "POST", path, eyes, 409)
		if answer["error"].(map[string]any)["status"] != "ALREADY_EXISTS" {
			t.Fatalf("on: %v", answer)
		}
		request(t, s, "POST", path, map[string]any{"emoji": map[string]string{"unicode": "\U0001F44D"}}, 200)
		request(t, s, "DELETE", "/v1/"+first["name"].(string), nil, 200)
		request(t, s, "POST", path, eyes, 200)
	}
}
