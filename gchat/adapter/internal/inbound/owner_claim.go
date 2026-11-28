package inbound

// Declarations only: the tests come first. The fixed lines of the turn, from
// the door's answer to a take.
var claimLines = map[string]string{
	"taken":         "[gchat: this sender has just become an owner of this box, by the one-time code from the owner page]",
	"already_owner": "[gchat: this message carried the live owner code and this sender was already an owner of this box; that code is now spent and nothing else changed]",
	"wrong":         "[gchat: this message has the shape of an owner code and is not the live one]",
	"tries_ended":   "[gchat: this message has the shape of an owner code and is not the live one; that code is now ended, a new one comes from the owner page]",
	"not_live":      "[gchat: this message has the shape of an owner code and no owner code is live; a new one comes from the owner page]",
	"full":          "[gchat: this message carried the live owner code and this box already has 50 owners; nobody was added and that code is now spent; one owner must be removed in Chat, and a new code comes from the owner page]",
	"no_answer":     "[gchat: this message has the shape of an owner code and this box could not check it]",
}
