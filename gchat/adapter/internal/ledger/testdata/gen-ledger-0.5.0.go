//go:build ignore

// gen-ledger-0.5.0 writes testdata/ledger-0.5.0.json with the real
// ledger.Save of the code it is built with. The committed fixture was written
// at gchat 0.5.0 (gascity-packs d2f453b83a428546dfe7fa01dc2bd831c0b47ae0),
// before the change record gained next_attempt, posted, cause and raised. It
// holds one change in each delivery state, the shapes a box running 0.5.0
// leaves behind. The record "crashwindow" is the one 0.5.0 leaves when it
// stops between the save that puts a fourth "not delivered" back in intent
// and the save that ends it as "delivery retries exhausted" (gp-dl26):
//
//	go run ./internal/ledger/testdata/gen-ledger-0.5.0.go internal/ledger/testdata/ledger-0.5.0.json
package main

import (
	"fmt"
	"os"
	"time"

	"gchat-adapter/internal/ledger"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-ledger-0.5.0 OUT")
		os.Exit(2)
	}
	at := time.Date(2026, 11, 10, 20, 22, 58, 0, time.UTC)
	l := ledger.New()
	l.IdentityUserID, l.IdentitySeed = "users/100", "users/100"
	l.Spaces["spaces/example"] = &ledger.SpaceState{Kind: "room", Wake: "all", MessageCursor: at, EventCursor: at, Backlog: []string{}}
	l.Messages["spaces/example/messages/one"] = &ledger.MessageState{Revision: at, Thread: "spaces/example/threads/one"}
	event := `{"provider_message_id":"%s","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/example","kind":"room"},"actor":{"id":"users/200","display_name":"Reader","is_bot":false},"text":"example","reply_to_message_id":"spaces/example/threads/one","received_at":"2026-11-10T20:22:58Z"}`
	for _, c := range []*ledger.ChangeState{
		{ID: "spaces/example/messages/intent", Status: "intent", Attempts: 2, Reason: "retry"},
		{ID: "spaces/example/messages/crashwindow", Status: "intent", Attempts: 4, Reason: "retry"},
		{ID: "spaces/example/messages/pending", Status: "pending", Attempts: 1, ReceiptID: "ir-1-1"},
		{ID: "spaces/example/messages/committed", Status: "committed", Attempts: 1, ReceiptID: "ir-1-2", Reason: "delivered"},
		{ID: "spaces/example/messages/unresolved", Status: "unresolved", Attempts: 4, ReceiptID: "ir-1-3", Reason: "delivery retries exhausted"},
	} {
		c.Space, c.Message, c.Kind, c.Created, c.ProvenanceFramed = "spaces/example", c.ID, "message.created", at, true
		c.Event = fmt.Sprintf(event, c.ID)
		l.Changes[c.ID] = c
		if c.Status == "unresolved" {
			copy := *c
			l.Unresolved[c.ID] = &copy
		}
	}
	l.Counters["turns:spaces/example"] = 1
	if err := l.Save(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
