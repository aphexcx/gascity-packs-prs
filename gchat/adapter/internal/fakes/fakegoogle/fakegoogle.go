// Package fakegoogle implements the Google endpoints used by the adapter.
// New starts a local HTTP server; Emit uses a publisher connected to pstest.
package fakegoogle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
	events "google.golang.org/api/workspaceevents/v1"
)

// UserID is the canonical authenticated user returned by message and reaction creates.
const UserID = "users/123456789"

// Server owns all fixtures. Set exported fields before making requests, or use
// Configure while clients are running. Configure callbacks must not call methods
// on Server. Fixtures are keyed by full resource name (spaces/room, etc.).
type Server struct {
	TokenError                 string
	OperationDelay             time.Duration
	OperationError             *events.Status
	CreateReturnsAlreadyExists bool
	DropNextResponse           bool // accept the next mutation, then close without a response
	EnforceRate                bool
	EmitRespectsSubscriptions  bool
	PageSize                   int
	Now                        func() time.Time
	Spaces                     map[string]*chat.Space
	Messages                   map[string]*chat.Message
	Reactions                  map[string]*chat.Reaction
	Subscriptions              map[string]*events.Subscription
	SpaceEvents                map[string][]*chat.SpaceEvent
	Media                      map[string][]byte
	// MaxUploadBytes lowers the normal 200 MiB limit for small boundary tests.
	MaxUploadBytes int64

	mu         sync.Mutex
	http       *httptest.Server
	t          testing.TB
	publisher  *pubsub.Publisher
	offset     time.Duration
	sequence   int
	operations map[string]*operation
	requests   map[string]string
	lastWrite  map[string]time.Time
	uploads    map[string]*resumableUpload
}

type resumableUpload struct {
	parent string
	data   []byte
	total  int64  // -1 until a total size is supplied
	name   string // populated only after completion
}

type operation struct {
	result       events.Operation
	failure      *events.Status
	ready        time.Time
	subscription *events.Subscription
	action       string
	sequence     int
}

// New permits a nil publisher for tests that do not call Emit. The caller owns
// the publisher and its pstest server; Close only closes the HTTP server.
func New(t testing.TB, publisher *pubsub.Publisher) *Server {
	t.Helper()
	s := &Server{t: t, publisher: publisher, Now: time.Now, PageSize: 100,
		Spaces: map[string]*chat.Space{}, Messages: map[string]*chat.Message{},
		Reactions: map[string]*chat.Reaction{}, Subscriptions: map[string]*events.Subscription{},
		SpaceEvents: map[string][]*chat.SpaceEvent{}, Media: map[string][]byte{},
		operations: map[string]*operation{}, requests: map[string]string{}, lastWrite: map[string]time.Time{}, uploads: map[string]*resumableUpload{}}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) URL() string                { return s.http.URL }
func (s *Server) Close()                     { s.http.Close() }
func (s *Server) Configure(fn func(*Server)) { s.mu.Lock(); defer s.mu.Unlock(); fn(s) }
func (s *Server) Advance(d time.Duration) {
	s.Configure(func(s *Server) { s.offset += d; s.refreshSubscriptions() })
}
func (s *Server) now() time.Time { return s.Now().Add(s.offset).UTC() }
func (s *Server) id() string     { s.sequence++; return strconv.Itoa(s.sequence) }

// SuspendSubscription suspends the named subscriptions, or all if none are
// named. Reactivation changes state without inventing a lifecycle event.
func (s *Server) SuspendSubscription(names ...string) {
	s.Configure(func(s *Server) {
		if len(names) == 0 {
			for name := range s.Subscriptions {
				names = append(names, name)
			}
		}
		for _, name := range names {
			if sub := s.Subscriptions[name]; sub != nil {
				sub.State = "SUSPENDED"
			}
		}
	})
}

func (s *Server) refreshSubscriptions() {
	// Complete ready Operations even if a create/renew response was lost and
	// the client reconciles by listing instead of polling. Apply them in time
	// order so advancing across both completion and expiry is deterministic.
	var pending []*operation
	for _, op := range s.operations {
		if !op.result.Done && !s.now().Before(op.ready) {
			pending = append(pending, op)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].ready.Equal(pending[j].ready) {
			return pending[i].sequence < pending[j].sequence
		}
		return pending[i].ready.Before(pending[j].ready)
	})
	for _, op := range pending {
		s.expireSubscriptions(op.ready)
		s.complete(op)
	}
	s.expireSubscriptions(s.now())
}

func (s *Server) expireSubscriptions(at time.Time) {
	for name, sub := range s.Subscriptions {
		if expiry, err := time.Parse(time.RFC3339Nano, sub.ExpireTime); err == nil && !at.Before(expiry) {
			delete(s.Subscriptions, name)
		}
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshSubscriptions()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.fail(w, 400, "INVALID_ARGUMENT", err.Error())
			return
		}
		if s.TokenError != "" {
			s.respond(w, r, 400, map[string]string{"error": s.TokenError})
			return
		}
		s.respond(w, r, 200, map[string]any{"access_token": "fake", "expires_in": 3600, "token_type": "Bearer", "scope": r.Form.Get("scope")})
	case strings.HasPrefix(path, "operations/") && r.Method == http.MethodGet:
		op := s.operations[path]
		if op == nil {
			s.fail(w, 404, "NOT_FOUND", "operation not found")
			return
		}
		s.respond(w, r, 200, &op.result)
	case path == "subscriptions" || strings.HasPrefix(path, "subscriptions/"):
		s.subscriptions(w, r, path)
	case strings.HasPrefix(r.URL.Path, "/upload/session/") && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		s.uploadChunk(w, r)
	case strings.HasPrefix(r.URL.Path, "/upload/v1/") && strings.HasSuffix(path, "attachments:upload") && r.Method == http.MethodPost:
		s.upload(w, r)
	case strings.HasPrefix(path, "media/") && r.Method == http.MethodGet:
		data, ok := s.Media[strings.TrimPrefix(path, "media/")]
		if !ok {
			s.fail(w, 404, "NOT_FOUND", "media not found")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	case path == "spaces" && r.Method == http.MethodGet:
		spaces := make([]*chat.Space, 0, len(s.Spaces))
		for _, v := range s.Spaces {
			spaces = append(spaces, v)
		}
		sort.Slice(spaces, func(i, j int) bool { return spaces[i].Name < spaces[j].Name })
		page(s, w, r, "spaces", spaces)
	case strings.HasPrefix(path, "spaces/"):
		s.chat(w, r, path)
	default:
		s.fail(w, 404, "NOT_FOUND", "route not found")
	}
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, body any) {
	if s.DropNextResponse && r.Method != http.MethodGet && status >= 200 && status < 300 {
		s.DropNextResponse = false
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			s.t.Errorf("drop response: %v", err)
			return
		}
		_ = conn.Close()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) fail(w http.ResponseWriter, code int, status, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "status": status, "message": message}})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func page[T any](s *Server, w http.ResponseWriter, r *http.Request, key string, items []T) {
	limit := s.PageSize
	if limit <= 0 {
		limit = 100
	}
	if raw := r.URL.Query().Get("pageSize"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid pageSize")
			return
		}
		limit = n
	}
	start := 0
	if raw := r.URL.Query().Get("pageToken"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > len(items) {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid pageToken")
			return
		}
		start = n
	}
	end := len(items)
	if limit < end-start {
		end = start + limit
	}
	result := map[string]any{key: items[start:end]}
	if end < len(items) {
		result["nextPageToken"] = strconv.Itoa(end)
	}
	s.respond(w, r, 200, result)
}

type queryFilter map[string][]string

var filterToken = regexp.MustCompile(`^(?:[():=<>]|"[^"\r\n]*"|[^\s():=<>!"&|]+)`)
var threadName = regexp.MustCompile(`^spaces/[^/\s]+/threads/[^/\s]+$`)

// parseFilter consumes the entire documented grammar: AND between constraints,
// OR only within one event_types group, and optional parentheses on that group.
// CamelCase field aliases remain supported for older fixtures.
func parseFilter(raw, route string) (queryFilter, error) {
	invalid := fmt.Errorf("invalid %s filter", route)
	var tokens []string
	for raw = strings.TrimSpace(raw); raw != ""; raw = strings.TrimSpace(raw) {
		token := filterToken.FindString(raw)
		if token == "" {
			return nil, invalid
		}
		tokens = append(tokens, token)
		raw = raw[len(token):]
	}
	f := queryFilter{}
	i := 0
	take := func(token string) bool {
		if i < len(tokens) && tokens[i] == token {
			i++
			return true
		}
		return false
	}
	atom := func() (string, error) {
		if i+3 > len(tokens) {
			return "", invalid
		}
		field, op, value := tokens[i], tokens[i+1], tokens[i+2]
		i += 3
		switch field {
		case "eventTypes":
			field = "event_types"
		case "startTime":
			field = "start_time"
		case "endTime":
			field = "end_time"
		case "createTime":
			field = "create_time"
		case "targetResource":
			field = "target_resource"
		}
		quoted := strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
		if quoted {
			value = value[1 : len(value)-1]
		}
		valid := false
		switch field {
		case "event_types":
			valid = route != "messages" && op == ":" && quoted
		case "target_resource":
			valid = route == "subscriptions" && op == "=" && quoted
		case "thread.name":
			valid = route == "messages" && op == "=" && !quoted && threadName.MatchString(value)
		case "create_time", "start_time", "end_time":
			_, err := time.Parse(time.RFC3339Nano, value)
			valid = quoted && err == nil && ((route == "messages" && field == "create_time" && (op == ">" || op == "<")) || (route == "spaceEvents" && field != "create_time" && op == "="))
		}
		key := field + op
		if !valid || value == "" || (field != "event_types" && len(f[key]) != 0) {
			return "", invalid
		}
		f[key] = append(f[key], value)
		return field, nil
	}
	seenEvents := false
	for i < len(tokens) {
		grouped := take("(")
		field, err := atom()
		if err != nil || (grouped && field != "event_types") {
			return nil, invalid
		}
		if field == "event_types" {
			if seenEvents {
				return nil, invalid // event_types AND event_types is invalid
			}
			seenEvents = true
		}
		for take("OR") {
			next, err := atom()
			if err != nil || field != "event_types" || next != "event_types" {
				return nil, invalid
			}
		}
		if grouped && !take(")") {
			return nil, invalid
		}
		if i == len(tokens) {
			break
		}
		if !take("AND") || i == len(tokens) {
			return nil, invalid
		}
	}
	if route != "messages" && !seenEvents {
		return nil, invalid
	}
	return f, nil
}

func (f queryFilter) time(key string) time.Time {
	if len(f[key]) == 0 {
		return time.Time{}
	}
	when, _ := time.Parse(time.RFC3339Nano, f[key][0]) // validated by parseFilter
	return when
}

func matches(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}

func (s *Server) subscriptions(w http.ResponseWriter, r *http.Request, path string) {
	if path == "subscriptions" {
		switch r.Method {
		case http.MethodPost:
			var sub events.Subscription
			if !s.decode(w, r, &sub) {
				return
			}
			if sub.TargetResource == "" {
				s.fail(w, 400, "INVALID_ARGUMENT", "targetResource is required")
				return
			}
			if len(sub.EventTypes) == 0 || sub.NotificationEndpoint == nil || sub.NotificationEndpoint.PubsubTopic == "" {
				s.fail(w, 400, "INVALID_ARGUMENT", "eventTypes and notificationEndpoint.pubsubTopic are required")
				return
			}
			for _, existing := range s.Subscriptions {
				if existing.TargetResource == sub.TargetResource {
					s.fail(w, 409, "ALREADY_EXISTS", "subscription exists")
					return
				}
			}
			for _, pending := range s.operations {
				if !pending.result.Done && pending.subscription.TargetResource == sub.TargetResource {
					s.fail(w, 409, "ALREADY_EXISTS", "subscription creation is pending")
					return
				}
			}
			if s.CreateReturnsAlreadyExists {
				s.fail(w, 409, "ALREADY_EXISTS", "scripted existing subscription")
				return
			}
			sub.Name = "subscriptions/" + s.id()
			sub.State = "ACTIVE"
			if !s.expiry(w, &sub) {
				return
			}
			s.newOperation(w, r, &sub, "create")
		case http.MethodGet:
			filter, err := parseFilter(r.URL.Query().Get("filter"), "subscriptions")
			if err != nil {
				s.fail(w, 400, "INVALID_ARGUMENT", err.Error())
				return
			}
			targets, types := filter["target_resource="], filter["event_types:"]
			list := make([]*events.Subscription, 0)
			for _, sub := range s.Subscriptions {
				if !matches(targets, sub.TargetResource) {
					continue
				}
				found := len(types) == 0
				for _, typ := range sub.EventTypes {
					if matches(types, typ) {
						found = true
					}
				}
				if found {
					list = append(list, sub)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
			page(s, w, r, "subscriptions", list)
		default:
			s.fail(w, 405, "INVALID_ARGUMENT", "method not allowed")
		}
		return
	}
	name := strings.TrimSuffix(path, ":reactivate")
	existing := s.Subscriptions[name]
	if existing == nil {
		s.fail(w, 404, "NOT_FOUND", "subscription not found")
		return
	}
	switch {
	case r.Method == http.MethodGet && name == path:
		s.respond(w, r, 200, existing)
	case r.Method == http.MethodPatch && name == path:
		var update events.Subscription
		if !s.decode(w, r, &update) {
			return
		}
		sub := *existing
		sub.Ttl = update.Ttl
		if !s.expiry(w, &sub) {
			return
		}
		s.newOperation(w, r, &sub, "patch")
	case r.Method == http.MethodPost && name != path:
		sub := *existing
		sub.State = "ACTIVE"
		sub.SuspensionReason = ""
		s.newOperation(w, r, &sub, "reactivate")
	default:
		s.fail(w, 405, "INVALID_ARGUMENT", "method not allowed")
	}
}

func (s *Server) expiry(w http.ResponseWriter, sub *events.Subscription) bool {
	ttl := 4 * time.Hour
	if sub.Ttl != "" && sub.Ttl != "0s" {
		d, err := time.ParseDuration(sub.Ttl)
		if err != nil || d < 0 || d > 4*time.Hour {
			s.fail(w, 400, "INVALID_ARGUMENT", "ttl must be at most four hours")
			return false
		}
		ttl = d
	}
	sub.ExpireTime = s.now().Add(ttl).Format(time.RFC3339Nano)
	sub.Ttl = ""
	return true
}

func (s *Server) newOperation(w http.ResponseWriter, r *http.Request, sub *events.Subscription, action string) {
	op := &operation{result: events.Operation{Name: "operations/" + s.id(), ForceSendFields: []string{"Done"}}, ready: s.now().Add(s.OperationDelay), subscription: sub}
	op.action, op.sequence = action, s.sequence
	if s.OperationError != nil {
		copy := *s.OperationError
		op.failure = &copy
	}
	s.operations[op.result.Name] = op
	s.complete(op)
	s.respond(w, r, 200, &op.result)
}

func (s *Server) complete(op *operation) {
	if op.result.Done || s.now().Before(op.ready) {
		return
	}
	op.result.Done = true
	op.result.Error = op.failure
	if op.result.Error == nil && op.action != "create" {
		current := s.Subscriptions[op.subscription.Name]
		if current == nil {
			op.result.Error = &events.Status{Code: 5, Message: "subscription not found"}
			return
		}
		merged := *current
		if op.action == "patch" {
			merged.ExpireTime = op.subscription.ExpireTime
		} else {
			merged.State = "ACTIVE"
			merged.SuspensionReason = ""
		}
		op.subscription = &merged
	}
	if op.result.Error == nil {
		s.Subscriptions[op.subscription.Name] = op.subscription
		b, _ := json.Marshal(op.subscription)
		var response map[string]json.RawMessage
		_ = json.Unmarshal(b, &response)
		response["@type"] = json.RawMessage(`"type.googleapis.com/google.apps.events.subscriptions.v1.Subscription"`)
		b, _ = json.Marshal(response)
		op.result.Response = googleapi.RawMessage(b)
	}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		s.fail(w, 404, "NOT_FOUND", "route not found")
		return
	}
	space := strings.Join(parts[:2], "/")
	if len(parts) == 3 && parts[2] == "spaceEvents" && r.Method == http.MethodGet {
		s.spaceEvents(w, r, space)
		return
	}
	if parts[2] != "messages" {
		s.fail(w, 404, "NOT_FOUND", "route not found")
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodPost:
			s.createMessage(w, r, space)
		case http.MethodGet:
			s.listMessages(w, r, space)
		default:
			s.fail(w, 405, "INVALID_ARGUMENT", "method not allowed")
		}
		return
	}
	name := strings.Join(parts[:4], "/")
	message := s.Messages[name]
	if message == nil {
		s.fail(w, 404, "NOT_FOUND", "message not found")
		return
	}
	if len(parts) == 4 && r.Method == http.MethodGet {
		s.respond(w, r, 200, message)
		return
	}
	if len(parts) >= 5 && parts[4] == "reactions" {
		switch {
		case len(parts) == 5 && r.Method == http.MethodPost:
			var reaction chat.Reaction
			if !s.decode(w, r, &reaction) {
				return
			}
			if reaction.Emoji == nil {
				s.fail(w, 400, "INVALID_ARGUMENT", "emoji required")
				return
			}
			reaction.Name = name + "/reactions/" + s.id()
			reaction.User = &chat.User{Name: UserID, Type: "HUMAN"}
			s.Reactions[reaction.Name] = &reaction
			s.respond(w, r, 200, &reaction)
		case len(parts) == 5 && r.Method == http.MethodGet:
			list := make([]*chat.Reaction, 0)
			for key, reaction := range s.Reactions {
				if strings.HasPrefix(key, name+"/reactions/") {
					list = append(list, reaction)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
			page(s, w, r, "reactions", list)
		case len(parts) == 6 && r.Method == http.MethodDelete:
			if s.Reactions[path] == nil {
				s.fail(w, 404, "NOT_FOUND", "reaction not found")
				return
			}
			delete(s.Reactions, path)
			s.respond(w, r, 200, map[string]any{})
		default:
			s.fail(w, 405, "INVALID_ARGUMENT", "method not allowed")
		}
		return
	}
	s.fail(w, 404, "NOT_FOUND", "route not found")
}

func (s *Server) createMessage(w http.ResponseWriter, r *http.Request, space string) {
	room := s.Spaces[space]
	if room == nil {
		s.fail(w, 404, "NOT_FOUND", "space not found")
		return
	}
	reply := r.URL.Query().Get("messageReplyOption")
	switch reply {
	case "", "MESSAGE_REPLY_OPTION_UNSPECIFIED", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", "REPLY_MESSAGE_OR_FAIL":
	default:
		s.fail(w, 400, "INVALID_ARGUMENT", "unknown messageReplyOption")
		return
	}
	requestID := r.URL.Query().Get("requestId")
	if name := s.requests[space+"\x00"+requestID]; requestID != "" && name != "" {
		s.respond(w, r, 200, s.Messages[name])
		return
	}
	var message chat.Message
	if !s.decode(w, r, &message) {
		return
	}
	if len(message.Text) > 32000 {
		s.fail(w, 400, "INVALID_ARGUMENT", "text exceeds 32000 bytes")
		return
	}
	if reply != "" && room.SpaceType != "SPACE" {
		s.fail(w, 400, "INVALID_ARGUMENT", "reply option requires a named space")
		return
	}
	if s.EnforceRate && s.now().Sub(s.lastWrite[space]) < time.Second {
		w.Header().Set("Retry-After", "1")
		s.fail(w, 429, "RESOURCE_EXHAUSTED", "one write per second per space")
		return
	}
	thread := ""
	if message.Thread != nil {
		thread = message.Thread.Name
	}
	if room.SpaceType == "SPACE" && (reply == "" || reply == "MESSAGE_REPLY_OPTION_UNSPECIFIED") {
		// The default starts a new thread, ignoring any supplied thread identity.
		thread = ""
	} else if room.SpaceType == "SPACE" {
		found := false
		for name, m := range s.Messages {
			if strings.HasPrefix(name, space+"/messages/") && m.Thread != nil && m.Thread.Name == thread {
				found = true
				break
			}
		}
		if !found {
			if reply == "REPLY_MESSAGE_OR_FAIL" {
				s.fail(w, 404, "NOT_FOUND", "thread not found")
				return
			}
			thread = ""
		}
	}
	if thread == "" {
		thread = space + "/threads/" + s.id()
	}
	message.Name = space + "/messages/" + s.id()
	message.Thread = &chat.Thread{Name: thread}
	message.CreateTime = s.now().Format(time.RFC3339Nano)
	message.Space = room
	message.Sender = &chat.User{Name: UserID, Type: "HUMAN"}
	s.Messages[message.Name] = &message
	s.lastWrite[space] = s.now()
	if requestID != "" {
		s.requests[space+"\x00"+requestID] = message.Name
	}
	s.respond(w, r, 200, &message)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, space string) {
	filter, err := parseFilter(r.URL.Query().Get("filter"), "messages")
	if err != nil {
		s.fail(w, 400, "INVALID_ARGUMENT", err.Error())
		return
	}
	threads := filter["thread.name="]
	since, until := filter.time("create_time>"), filter.time("create_time<")
	list := make([]*chat.Message, 0)
	for name, m := range s.Messages {
		if !strings.HasPrefix(name, space+"/messages/") {
			continue
		}
		thread := ""
		if m.Thread != nil {
			thread = m.Thread.Name
		}
		if !matches(threads, thread) {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
		if !since.IsZero() && !created.After(since) {
			continue
		}
		if !until.IsZero() && !created.Before(until) {
			continue
		}
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, list[i].CreateTime)
		b, _ := time.Parse(time.RFC3339Nano, list[j].CreateTime)
		if a.Equal(b) {
			return list[i].Name < list[j].Name
		}
		return a.Before(b)
	})
	if strings.Contains(r.URL.Query().Get("orderBy"), "desc") {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	page(s, w, r, "messages", list)
}

func singularEvent(typ string) string {
	for _, suffix := range []string{"Created", "Updated", "Deleted"} {
		typ = strings.ReplaceAll(typ, ".batch"+suffix, "."+strings.ToLower(suffix))
	}
	return typ
}

func (s *Server) spaceEvents(w http.ResponseWriter, r *http.Request, space string) {
	filter, err := parseFilter(r.URL.Query().Get("filter"), "spaceEvents")
	if err != nil {
		s.fail(w, 400, "INVALID_ARGUMENT", err.Error())
		return
	}
	types := filter["event_types:"]
	since, until := filter.time("start_time="), filter.time("end_time=")
	earliest := s.now().Add(-28 * 24 * time.Hour)
	if !since.IsZero() && since.Before(earliest) {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid time filter or start_time older than 28 days")
		return
	}
	if since.IsZero() {
		since = earliest
	}
	if until.IsZero() || until.After(s.now()) {
		until = s.now()
	}
	list := make([]*chat.SpaceEvent, 0)
	for _, event := range s.SpaceEvents[space] {
		when, err := time.Parse(time.RFC3339Nano, event.EventTime)
		if err != nil || !when.After(since) || when.After(until) || !matches(types, singularEvent(event.EventType)) {
			continue
		}
		list = append(list, event)
	}
	sort.Slice(list, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, list[i].EventTime)
		b, _ := time.Parse(time.RFC3339Nano, list[j].EventTime)
		if a.Equal(b) {
			return list[i].Name < list[j].Name
		}
		return a.Before(b)
	})
	// Retention can remove earlier events between pages. Resume from the last
	// event's ordered position, never an offset into the shrinking result set.
	if token := r.URL.Query().Get("pageToken"); token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		var position [2]string
		if err != nil || json.Unmarshal(raw, &position) != nil || position[1] == "" {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid pageToken")
			return
		}
		after, err := time.Parse(time.RFC3339Nano, position[0])
		if err != nil {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid pageToken")
			return
		}
		start := sort.Search(len(list), func(i int) bool {
			when, _ := time.Parse(time.RFC3339Nano, list[i].EventTime)
			return when.After(after) || (when.Equal(after) && list[i].Name > position[1])
		})
		list = list[start:]
	}
	limit := s.PageSize
	if limit <= 0 {
		limit = 100
	}
	if raw := r.URL.Query().Get("pageSize"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid pageSize")
			return
		}
		limit = n
	}
	result := map[string]any{"spaceEvents": list[:min(limit, len(list))]}
	if len(list) > limit {
		last := list[limit-1]
		raw, _ := json.Marshal([2]string{last.EventTime, last.Name})
		result["nextPageToken"] = base64.RawURLEncoding.EncodeToString(raw)
	}
	s.respond(w, r, 200, result)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	parent := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload/v1/"), "/attachments:upload")
	if s.Spaces[parent] == nil {
		s.fail(w, 404, "NOT_FOUND", "space not found")
		return
	}
	if r.URL.Query().Get("uploadType") == "resumable" {
		var metadata chat.UploadAttachmentRequest
		if !s.decode(w, r, &metadata) {
			return
		}
		upload := &resumableUpload{parent: parent, total: -1}
		if raw := r.Header.Get("X-Upload-Content-Length"); raw != "" {
			total, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || total < 0 || total > s.mediaLimit() {
				s.fail(w, 400, "INVALID_ARGUMENT", "invalid upload size")
				return
			}
			upload.total = total
		}
		path := "/upload/session/" + s.id()
		s.uploads[path] = upload
		w.Header().Set("Location", s.URL()+path)
		s.respond(w, r, 200, map[string]any{})
		return
	}
	kind, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid content type")
		return
	}
	var data []byte
	if strings.HasPrefix(kind, "multipart/") {
		reader := multipart.NewReader(r.Body, params["boundary"])
		metadata, e := reader.NextPart()
		if e != nil {
			s.fail(w, 400, "INVALID_ARGUMENT", "missing upload metadata")
			return
		}
		_, e = io.Copy(io.Discard, metadata)
		if e != nil {
			s.fail(w, 400, "INVALID_ARGUMENT", e.Error())
			return
		}
		part, e := reader.NextPart()
		if e != nil {
			s.fail(w, 400, "INVALID_ARGUMENT", "missing upload data")
			return
		}
		data, err = io.ReadAll(io.LimitReader(part, s.mediaLimit()+1))
	} else {
		data, err = io.ReadAll(io.LimitReader(r.Body, s.mediaLimit()+1))
	}
	if err != nil {
		s.fail(w, 400, "INVALID_ARGUMENT", err.Error())
		return
	}
	if int64(len(data)) > s.mediaLimit() {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid upload size")
		return
	}
	name := parent + "/attachments/" + s.id()
	s.Media[name] = data
	s.respond(w, r, 200, &chat.UploadAttachmentResponse{AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: name}})
}

const maxUploadBytes = 200 << 20

func (s *Server) mediaLimit() int64 {
	if s.MaxUploadBytes > 0 && s.MaxUploadBytes < maxUploadBytes {
		return s.MaxUploadBytes
	}
	return maxUploadBytes
}

var contentRange = regexp.MustCompile(`^bytes (?:([0-9]+)-([0-9]+)|(\*))/([0-9]+|\*)$`)

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	upload := s.uploads[r.URL.Path]
	if upload == nil {
		s.fail(w, 404, "NOT_FOUND", "upload session not found")
		return
	}
	rangeParts := contentRange.FindStringSubmatch(r.Header.Get("Content-Range"))
	if rangeParts == nil {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid Content-Range")
		return
	}
	total := upload.total
	if rangeParts[4] != "*" {
		value, err := strconv.ParseInt(rangeParts[4], 10, 64)
		if err != nil || value > s.mediaLimit() || value < int64(len(upload.data)) || (total >= 0 && total != value) {
			s.fail(w, 400, "INVALID_ARGUMENT", "inconsistent upload size")
			return
		}
		total = value
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, s.mediaLimit()+1))
	if err != nil || int64(len(data)) > s.mediaLimit() {
		s.fail(w, 400, "INVALID_ARGUMENT", "invalid upload chunk")
		return
	}
	if rangeParts[3] == "*" {
		if len(data) != 0 || (total >= 0 && int64(len(upload.data)) > total) {
			s.fail(w, 400, "INVALID_ARGUMENT", "invalid upload status query")
			return
		}
	} else {
		start, startErr := strconv.ParseInt(rangeParts[1], 10, 64)
		end, endErr := strconv.ParseInt(rangeParts[2], 10, 64)
		if startErr != nil || endErr != nil || end < start || end >= s.mediaLimit() || end-start+1 != int64(len(data)) || (total >= 0 && end >= total) {
			s.fail(w, 400, "INVALID_ARGUMENT", "chunk does not match Content-Range")
			return
		}
		switch {
		case start == int64(len(upload.data)) && upload.name == "":
			upload.data = append(upload.data, data...)
		case end < int64(len(upload.data)) && bytes.Equal(upload.data[start:end+1], data):
			// A lost response may cause an identical chunk to be retried.
		default:
			s.fail(w, 400, "INVALID_ARGUMENT", "chunk is not contiguous with uploaded data")
			return
		}
	}
	upload.total = total
	if total >= 0 && int64(len(upload.data)) == total {
		if upload.name == "" {
			upload.name = upload.parent + "/attachments/" + s.id()
			s.Media[upload.name] = upload.data
		}
		s.respond(w, r, 200, &chat.UploadAttachmentResponse{AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: upload.name}})
		return
	}
	if len(upload.data) > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(upload.data)-1))
	}
	status := 308 // Google's Resume Incomplete, not a redirect.
	if r.Header.Get("X-GUploader-No-308") == "yes" {
		w.Header().Set("X-HTTP-Status-Code-Override", "308")
		status = 200
	}
	s.respond(w, r, status, map[string]any{})
}

// Emit publishes a binary-mode CloudEvent. For a batch event resource is a slice
// of resources; each becomes a singular wrapper inside the plural array. The
// Pub/Sub message data is the event payload, not a second CloudEvent envelope.
// Subscription lifecycle events wrap the subscription and use it as the subject.
// By default Emit ignores subscription state so fixtures can inject missed or
// delayed events. With EmitRespectsSubscriptions, it drops events unless an
// active, unexpired subscription covers the space and event type (including
// batch variants). Lifecycle types are implicit on every subscription.
func (s *Server) Emit(eventType string, resource any, space string) {
	s.t.Helper()
	s.mu.Lock()
	allowed := true
	if s.EmitRespectsSubscriptions {
		s.refreshSubscriptions()
		allowed = false
		target := space
		if !strings.HasPrefix(target, "//") {
			target = "//chat.googleapis.com/" + space
		}
		for _, sub := range s.Subscriptions {
			if sub.State == "ACTIVE" && (sub.TargetResource == target || sub.TargetResource == "//chat.googleapis.com/spaces/-") &&
				(strings.HasPrefix(eventType, "google.workspace.events.subscription.v1.") || (len(sub.EventTypes) > 0 && matches(sub.EventTypes, singularEvent(eventType)))) {
				allowed = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !allowed {
		return
	}
	if s.publisher == nil {
		s.t.Error("fakegoogle.Emit requires a pstest-connected publisher")
		return
	}
	wrapper := ""
	for _, kind := range []string{"message", "reaction", "membership"} {
		if strings.Contains(eventType, "."+kind+".v1.") {
			wrapper = kind
			break
		}
	}
	if strings.HasPrefix(eventType, "google.workspace.events.subscription.v1.") {
		wrapper = "subscription"
	}
	body := resource
	if wrapper != "" {
		if strings.Contains(eventType, ".batch") {
			value := reflect.ValueOf(resource)
			if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
				s.t.Errorf("batch event requires a slice, got %T", resource)
				return
			}
			items := make([]map[string]any, 0, value.Len())
			for i := 0; i < value.Len(); i++ {
				items = append(items, map[string]any{wrapper: value.Index(i).Interface()})
			}
			body = map[string]any{wrapper + "s": items}
		} else {
			body = map[string]any{wrapper: resource}
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		s.t.Errorf("marshal event: %v", err)
		return
	}
	s.mu.Lock()
	id := s.id()
	when := s.now().Format(time.RFC3339Nano)
	s.mu.Unlock()
	subject := space
	if wrapper == "subscription" {
		var payload struct {
			Subscription struct{ Name string }
		}
		_ = json.Unmarshal(data, &payload)
		if payload.Subscription.Name != "" {
			subject = payload.Subscription.Name
		}
		if !strings.HasPrefix(subject, "//") {
			subject = "//workspaceevents.googleapis.com/" + subject
		}
	} else if !strings.HasPrefix(subject, "//") {
		subject = "//chat.googleapis.com/" + space
	}
	msg := &pubsub.Message{Data: data, Attributes: map[string]string{"ce-type": eventType, "ce-subject": subject, "ce-id": "event-" + id, "ce-time": when, "ce-specversion": "1.0", "ce-source": "//workspaceevents.googleapis.com/subscriptions/example"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.publisher.Publish(ctx, msg).Get(ctx); err != nil {
		s.t.Error(fmt.Errorf("publish event: %w", err))
	}
}
