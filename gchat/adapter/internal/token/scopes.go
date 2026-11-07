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

// KeyholderScopes is what a managed Source asks its key holder for: the Chat
// and Pub/Sub scopes above without openid (an OAuth consent scope that a
// delegated token has no use for), plus the directory read that resolves the
// identity's own Chat id. Callers must not modify it.
var KeyholderScopes = []string{
	"https://www.googleapis.com/auth/chat.messages",
	"https://www.googleapis.com/auth/chat.spaces.readonly",
	"https://www.googleapis.com/auth/chat.memberships.readonly",
	"https://www.googleapis.com/auth/pubsub",
	"https://www.googleapis.com/auth/directory.readonly",
}
