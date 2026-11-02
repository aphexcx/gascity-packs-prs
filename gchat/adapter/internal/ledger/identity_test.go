package ledger_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gchat-adapter/internal/ledger"
)

func TestOptionalIdentityRoundTripAndLegacy(t *testing.T) {
	for _, identity := range []string{"", "users/222"} {
		t.Run(identity, func(t *testing.T) {
			l := ledger.New()
			l.IdentityUserID = identity
			if identity != "" {
				l.IdentitySeed = "users/111"
			}
			path := filepath.Join(t.TempDir(), "state.json")
			if err := l.Save(path); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if identity == "" && (fields["identity_user_id"] != nil || fields["identity_seed"] != nil) {
				t.Fatal("empty optional identity serialized")
			}
			got, err := ledger.Load(path, true)
			if err != nil || got.IdentityUserID != l.IdentityUserID || got.IdentitySeed != l.IdentitySeed {
				t.Fatalf("round trip: %v %+v", err, got)
			}
		})
	}
}
