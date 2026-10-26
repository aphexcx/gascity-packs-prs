package token

// Scopes is the shared consent scope list for the adapter's Chat reads/writes,
// memberships, Pub/Sub subscriber and authenticated user identity. Callers must not modify it.
var Scopes = []string{
	"https://www.googleapis.com/auth/chat.messages",
	"https://www.googleapis.com/auth/chat.spaces.readonly",
	"https://www.googleapis.com/auth/chat.memberships.readonly",
	"https://www.googleapis.com/auth/pubsub",
	"openid",
}
