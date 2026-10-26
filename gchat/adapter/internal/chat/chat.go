// Package chat wraps the authenticated Google Chat API with bounded retries,
// write pacing, and filesystem boundaries for media transfers.
package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gchat-adapter/internal/config"
	googlechat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type Space = googlechat.Space

// Client belongs to one authenticated identity. Do not reuse it after switching
// credentials: its reaction ownership and rate limits belong to that identity.
type Client struct {
	api                    *googlechat.Service
	limits                 Limiter
	timing                 timing
	retryBound             time.Duration
	ep                     config.Endpoints
	identityMu             sync.RWMutex
	identityUserID         string
	observedIdentityUserID string
	identityMismatch       error
}

func New(ctx context.Context, httpClient *http.Client, ep config.Endpoints) (*Client, error) {
	if httpClient == nil {
		return nil, errors.New("chat: authenticated HTTP client is required")
	}
	endpoint := ep.ChatAPIBase
	if endpoint == "" {
		endpoint = "https://chat.googleapis.com"
	}
	api, err := googlechat.NewService(ctx, option.WithEndpoint(strings.TrimRight(endpoint, "/")+"/"), option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return &Client{api: api, ep: ep}, nil
}

// SetIdentityUserID supplies the canonical users/<id> from the authenticated
// identity. Configure it at startup so DeleteReaction also works before this
// client has created a reaction. A create response can discover it otherwise.
func (c *Client) SetIdentityUserID(id string) error {
	if !validUserID.MatchString(id) {
		return localError{errors.New("chat: canonical identity user ID required")}
	}
	c.identityMu.Lock()
	defer c.identityMu.Unlock()
	return c.setIdentityUserID(id)
}

// setIdentityUserID requires identityMu; observation and conflict detection
// must become visible together to the app's persistence snapshot.
func (c *Client) setIdentityUserID(id string) error {
	if c.identityUserID != "" && c.identityUserID != id {
		if c.identityMismatch == nil {
			c.identityMismatch = localError{errors.New("chat: identity user ID mismatch; rebuild the client before deleting reactions")}
			slog.Warn("chat identity user ID mismatch; rebuild the client before deleting reactions", "expected_user_id", c.identityUserID, "observed_user_id", id)
		}
		return c.identityMismatch
	}
	c.identityUserID = id
	return nil
}

var validUserID = regexp.MustCompile(`^users/[0-9]+$`)

// Learning cannot turn a stored create into a failed send. A canonical conflict
// still disables reaction removal until the app rebuilds this client.
func (c *Client) learnIdentityUserID(id string) {
	if validUserID.MatchString(id) {
		c.identityMu.Lock()
		defer c.identityMu.Unlock()
		c.observedIdentityUserID = id
		_ = c.setIdentityUserID(id)
	}
}

func (c *Client) reactionIdentity() (string, error) {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	if c.identityMismatch != nil {
		return "", c.identityMismatch
	}
	if c.identityUserID == "" {
		return "", localError{errors.New("chat: SetIdentityUserID is required before deleting reactions")}
	}
	return c.identityUserID, nil
}

// IdentityUserID returns the canonical user resource learned from a successful
// create response or supplied at startup. The app can persist it in its ledger.
func (c *Client) IdentityUserID() string {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	return c.identityUserID
}

// ObservedIdentityUserID lets the app persist the server-observed identity and
// rebuild after a conflict. IdentityUserID deliberately retains its original
// value so a conflicted client cannot silently change reaction ownership.
func (c *Client) ObservedIdentityUserID() (id string, conflicted bool) {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	id = c.observedIdentityUserID
	if id == "" {
		id = c.identityUserID
	}
	return id, c.identityMismatch != nil
}

func (c *Client) ListSpaces(ctx context.Context) ([]Space, error) {
	var spaces []Space
	call := c.api.Spaces.List().PageSize(1000)
	for {
		var result *googlechat.ListSpacesResponse
		err := c.retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
		if err != nil {
			return nil, err
		}
		for _, space := range result.Spaces {
			spaces = append(spaces, *space)
		}
		if result.NextPageToken == "" {
			return spaces, nil
		}
		call.PageToken(result.NextPageToken)
	}
}

func (c *Client) CreateMessage(ctx context.Context, space, text, thread, requestID string, named bool) (name, threadName string, err error) {
	return c.CreateMessageWithAttachments(ctx, space, text, thread, requestID, named, nil)
}

// CreateMessageWithAttachments posts previously uploaded attachment references.
// The same requestID must be reused on retries, including retries after restart.
func (c *Client) CreateMessageWithAttachments(ctx context.Context, space, text, thread, requestID string, named bool, attachments []*googlechat.Attachment) (name, threadName string, err error) {
	message := &googlechat.Message{Text: text, Attachment: attachments}
	if thread != "" {
		message.Thread = &googlechat.Thread{Name: thread}
	}
	call := c.api.Spaces.Messages.Create(space, message)
	if requestID != "" {
		call.RequestId(requestID)
	}
	if named {
		call.MessageReplyOption("REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	}
	var result *googlechat.Message
	err = c.retry(ctx, requestID != "", func(ctx context.Context) error {
		if err := c.limits.WaitWrite(ctx, space); err != nil {
			return err
		}
		var err error
		result, err = call.Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", "", err
	}
	if result.Thread != nil {
		threadName = result.Thread.Name
	}
	// Replayed creates may omit the sender. Never erase a previously learned ID.
	if result.Sender != nil && result.Sender.Name != "" {
		c.learnIdentityUserID(result.Sender.Name)
	}
	return result.Name, threadName, nil
}

func (c *Client) ListMessages(ctx context.Context, space string, since time.Time, thread string, page func([]*googlechat.Message) error) error {
	if page == nil {
		return localError{errors.New("chat: message page callback is required")}
	}
	var filters []string
	if !since.IsZero() {
		filters = append(filters, "create_time > "+strconv.Quote(since.UTC().Format(time.RFC3339Nano)))
	}
	if thread != "" {
		if !validThread.MatchString(thread) {
			return localError{errors.New("chat: invalid thread resource name")}
		}
		filters = append(filters, "thread.name = "+thread)
	}
	call := c.api.Spaces.Messages.List(space).PageSize(1000).Filter(strings.Join(filters, " AND "))
	for {
		var result *googlechat.ListMessagesResponse
		err := c.retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
		if err != nil {
			return err
		}
		if err := page(result.Messages); err != nil {
			return err
		}
		if result.NextPageToken == "" {
			return nil
		}
		call.PageToken(result.NextPageToken)
	}
}

var validThread = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/threads/[A-Za-z0-9_-]+$`)

// ListMessagesPage reads one page of a frozen, exclusive create-time interval.
// Persist nextPageToken with these same bounds before requesting another page.
func (c *Client) ListMessagesPage(ctx context.Context, space string, lower, upper time.Time, thread, pageToken string) (msgs []*googlechat.Message, nextPageToken string, err error) {
	if lower.IsZero() || upper.IsZero() || !lower.Before(upper) {
		return nil, "", localError{errors.New("chat: both frozen bounds must define a nonempty interval")}
	}
	filters := []string{"create_time > " + strconv.Quote(lower.UTC().Format(time.RFC3339Nano)), "create_time < " + strconv.Quote(upper.UTC().Format(time.RFC3339Nano))}
	if thread != "" {
		if !validThread.MatchString(thread) {
			return nil, "", localError{errors.New("chat: invalid thread resource name")}
		}
		filters = append(filters, "thread.name = "+thread)
	}
	call := c.api.Spaces.Messages.List(space).PageSize(1000).OrderBy("create_time ASC").Filter(strings.Join(filters, " AND ")).PageToken(pageToken)
	var result *googlechat.ListMessagesResponse
	err = c.retry(ctx, true, func(ctx context.Context) error {
		var err error
		result, err = call.Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return result.Messages, result.NextPageToken, nil
}

func (c *Client) GetMessage(ctx context.Context, name string) (*googlechat.Message, error) {
	var result *googlechat.Message
	err := c.retry(ctx, true, func(ctx context.Context) error {
		var err error
		result, err = c.api.Spaces.Messages.Get(name).Context(ctx).Do()
		return err
	})
	return result, err
}

func (c *Client) ListSpaceEvents(ctx context.Context, space string, since time.Time, types []string, page func([]*googlechat.SpaceEvent) error) error {
	if page == nil || len(types) == 0 {
		return localError{errors.New("chat: event types and page callback are required")}
	}
	var terms []string
	for _, typ := range types {
		terms = append(terms, "event_types:"+strconv.Quote(typ))
	}
	filter := "(" + strings.Join(terms, " OR ") + ")"
	if !since.IsZero() {
		filter = "start_time=" + strconv.Quote(since.UTC().Format(time.RFC3339Nano)) + " AND " + filter
	}
	call := c.api.Spaces.SpaceEvents.List(space).PageSize(1000).Filter(filter)
	for {
		var result *googlechat.ListSpaceEventsResponse
		err := c.retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
		if err != nil {
			return err
		}
		if err := page(result.SpaceEvents); err != nil {
			return err
		}
		if result.NextPageToken == "" {
			return nil
		}
		call.PageToken(result.NextPageToken)
	}
}

// ListSpaceEventsPage reads one chronological page between frozen event-time
// bounds. The API orders events itself and has no orderBy parameter.
func (c *Client) ListSpaceEventsPage(ctx context.Context, space string, lower, upper time.Time, types []string, pageToken string) (events []*googlechat.SpaceEvent, nextPageToken string, err error) {
	if upper.IsZero() || (!lower.IsZero() && !lower.Before(upper)) || len(types) == 0 {
		return nil, "", localError{errors.New("chat: event types and a nonempty frozen interval are required")}
	}
	var terms []string
	for _, typ := range types {
		terms = append(terms, "event_types:"+strconv.Quote(typ))
	}
	filter := "end_time=" + strconv.Quote(upper.UTC().Format(time.RFC3339Nano)) + " AND (" + strings.Join(terms, " OR ") + ")"
	// A zero lower bound uses Google's retained window, avoiding clock skew
	// or request transit making an explicit retention boundary too old.
	if !lower.IsZero() {
		filter = "start_time=" + strconv.Quote(lower.UTC().Format(time.RFC3339Nano)) + " AND " + filter
	}
	call := c.api.Spaces.SpaceEvents.List(space).PageSize(1000).Filter(filter).PageToken(pageToken)
	var result *googlechat.ListSpaceEventsResponse
	err = c.retry(ctx, true, func(ctx context.Context) error {
		var err error
		result, err = call.Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return result.SpaceEvents, result.NextPageToken, nil
}

func (c *Client) CreateReaction(ctx context.Context, message, emoji string) (string, error) {
	var result *googlechat.Reaction
	// Reaction creates have no request ID. Only confirmed quota rejections are
	// retried, never an ambiguous transport failure after the server accepted it.
	err := c.retry(ctx, false, func(ctx context.Context) error {
		if err := c.limits.WaitReaction(ctx); err != nil {
			return err
		}
		var err error
		result, err = c.api.Spaces.Messages.Reactions.Create(message, &googlechat.Reaction{Emoji: &googlechat.Emoji{Unicode: emoji}}).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", err
	}
	if result.User != nil && result.User.Name != "" {
		c.learnIdentityUserID(result.User.Name)
	}
	return result.Name, nil
}

func (c *Client) DeleteReaction(ctx context.Context, message, emoji string) error {
	user, err := c.reactionIdentity()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	call := c.api.Spaces.Messages.Reactions.List(message).PageSize(1000)
	// Collect before deleting: deleting while paging can shift page boundaries.
	var names []string
	for {
		var result *googlechat.ListReactionsResponse
		err := c.retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
		if err != nil {
			return err
		}
		for _, reaction := range result.Reactions {
			if reaction.User != nil && reaction.User.Name == user && reaction.Emoji != nil && reaction.Emoji.Unicode == emoji {
				names = append(names, reaction.Name)
			}
		}
		if result.NextPageToken == "" {
			break
		}
		call.PageToken(result.NextPageToken)
	}
	for _, name := range names {
		err := c.retry(ctx, true, func(ctx context.Context) error {
			if err := c.limits.WaitReaction(ctx); err != nil {
				return err
			}
			// A concurrent create may discover a conflict while listing or pacing.
			if _, err := c.reactionIdentity(); err != nil {
				return err
			}
			_, err := c.api.Spaces.Messages.Reactions.Delete(name).Context(ctx).Do()
			var apiErr *googleapi.Error
			if errors.As(err, &apiErr) && apiErr.Code == 404 {
				return nil
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) UploadAttachment(ctx context.Context, space, path string) (*googlechat.AttachmentDataRef, error) {
	file, parent, err := openMediaFile(c.ep.UploadRoot, path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("upload %q: %w", path, localError{err})
	}
	defer parent.Close()
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("upload %q: %w", path, localError{err})
	}
	limit := int64(200 * 1024 * 1024)
	if c.ep.MediaMaxBytes > 0 && c.ep.MediaMaxBytes < limit {
		limit = c.ep.MediaMaxBytes
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("upload %q: %w", path, localError{errors.New("file exceeds media limit")})
	}
	var result *googlechat.UploadAttachmentResponse
	err = c.retry(ctx, false, func(waitCtx context.Context) error {
		if err := c.limits.WaitWrite(waitCtx, space); err != nil {
			return err
		}
		// Recheck on each attempt: the file may grow during a quota wait.
		info, err := file.Stat()
		if err != nil {
			return localError{err}
		}
		if info.Size() > limit {
			return localError{errors.New("file exceeds media limit")}
		}
		// Stream one multipart request, including for Chat's 200 MB maximum.
		// SDK resumable chunks retry 429s internally without Retry-After and lose
		// the HTTP error on deadline. Keep every retry under our policy instead.
		transferCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stream := newUploadReader(file, limit, cancel)
		result, err = c.api.Media.Upload(space, &googlechat.UploadAttachmentRequest{Filename: filepath.Base(path)}).Media(stream, googleapi.ChunkSize(0)).Context(transferCtx).Do()
		// The SDK formats source errors as strings. Preserve the local byte-limit
		// error so a growing file cannot yield a success or a transient receipt.
		if limitErr := stream.Err(); limitErr != nil {
			return limitErr
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("upload %q: %w", path, err)
	}
	if result.AttachmentDataRef == nil {
		return nil, fmt.Errorf("upload %q: missing attachment reference", path)
	}
	return result.AttachmentDataRef, nil
}

// Unlike LimitReader, uploadReader reports an oversized stream instead of
// silently truncating it into a successful attachment if the file grows.
type uploadReader struct {
	reader    io.Reader
	remaining int64
	cancel    context.CancelFunc
	mu        sync.Mutex
	err       error
}

func newUploadReader(file *os.File, limit int64, cancel context.CancelFunc) *uploadReader {
	// Each SDK pipe gets its own offset, including any earlier pipe still exiting.
	return &uploadReader{reader: io.NewSectionReader(file, 0, limit+1), remaining: limit, cancel: cancel}
}

func (r *uploadReader) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n > 0 {
			err := localError{errors.New("file exceeds media limit")}
			r.mu.Lock()
			r.err = err
			r.mu.Unlock()
			// Abort before the SDK closes the multipart boundary around a prefix.
			r.cancel()
			return 0, err
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

// DownloadMedia creates a new private file below MediaDir, never overwrites an
// existing file, and removes an incomplete download. The caller creates parent
// directories and only uses dest after success. No component below MediaDir may be a link.
func (c *Client) DownloadMedia(ctx context.Context, resourceName, dest string, maxBytes int64) (n int64, err error) {
	if maxBytes <= 0 {
		return 0, localError{errors.New("chat: positive download byte limit required")}
	}
	if c.ep.MediaMaxBytes > 0 && maxBytes > c.ep.MediaMaxBytes {
		maxBytes = c.ep.MediaMaxBytes
	}
	var response *http.Response
	// Transfers, including reading the body after retry returns, use the caller's
	// deadline. Only pacing and retries are bounded by the publish wait budget.
	err = c.retry(ctx, true, func(_ context.Context) error {
		var err error
		response, err = c.api.Media.Download(resourceName).Context(ctx).Download()
		return err
	})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.ContentLength > maxBytes {
		return 0, localError{errors.New("chat: media exceeds download byte limit")}
	}
	file, parent, err := openMediaFile(c.ep.MediaDir, dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, localError{err}
	}
	defer parent.Close()
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			err = errors.Join(err, removeMediaFile(parent, filepath.Base(dest)))
		}
	}()
	n, err = io.CopyN(file, response.Body, maxBytes)
	if errors.Is(err, io.EOF) {
		err = nil
		return n, err
	}
	if err != nil {
		return n, err
	}
	var extra [1]byte
	_, err = io.ReadFull(response.Body, extra[:])
	if errors.Is(err, io.EOF) {
		return n, nil
	}
	if err == nil {
		err = localError{errors.New("chat: media exceeds download byte limit")}
	}
	return n, err
}

type localError struct{ error }

func (e localError) Unwrap() error { return e.error }

// Classify maps errors to gc receipt kinds. Retry-After accepts both delay
// seconds and an HTTP date; malformed or past hints produce a zero delay.
func Classify(err error) (kind string, retryAfter time.Duration) {
	return classifyAt(err, time.Now())
}

func classifyAt(err error, now time.Time) (kind string, retryAfter time.Duration) {
	if err == nil {
		return "", 0
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		hint := apiErr.Header.Get("Retry-After")
		if seconds, e := strconv.ParseInt(hint, 10, 64); e == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			retryAfter = time.Duration(seconds) * time.Second
		} else if when, e := http.ParseTime(hint); e == nil && when.After(now) {
			retryAfter = when.Sub(now)
		}
		switch {
		case apiErr.Code == 429:
			return "rate_limited", retryAfter
		case apiErr.Code == 403 && strings.Contains(strings.ToLower(apiErr.Message), "not a member"):
			return "not_found", retryAfter
		case apiErr.Code == 401 || apiErr.Code == 403:
			return "auth", retryAfter
		case apiErr.Code == 404:
			return "not_found", retryAfter
		case apiErr.Code == 408 || apiErr.Code == 409:
			return "transient", retryAfter
		case apiErr.Code >= 400 && apiErr.Code < 500:
			return "permanent", retryAfter
		default:
			return "transient", retryAfter
		}
	}
	var local localError
	if errors.As(err, &local) {
		return "permanent", 0
	}
	return "transient", 0
}

// retry bounds pacing and backoff. Ordinary calls also use its bounded context;
// media calls use the caller's context for I/O so large transfers can finish.
// Only safe operations retry ambiguous failures. A deadline preserves the last
// API error and retry hint.
func (c *Client) retry(ctx context.Context, safe bool, call func(context.Context) error) error {
	bound := c.retryBound
	if bound <= 0 {
		bound = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	deadline := c.timing.Now().Add(bound)
	remaining := func() time.Duration {
		d := deadline.Sub(c.timing.Now())
		if realDeadline, ok := ctx.Deadline(); c.timing.now == nil && ok && time.Until(realDeadline) < d {
			d = time.Until(realDeadline)
		}
		return d
	}
	backoff := time.Second
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		if remaining() <= 0 {
			return errors.Join(last, context.DeadlineExceeded)
		}
		err := call(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(last, err, ctx.Err())
		}
		kind, hint := classifyAt(err, c.timing.Now())
		if kind != "rate_limited" && !(kind == "transient" && safe) {
			return err
		}
		last = err
		if hint < backoff {
			hint = backoff
		}
		// Return the server's error immediately if the required wait cannot fit.
		// In particular, never discard Retry-After for a bare deadline error.
		if hint >= remaining() {
			return errors.Join(last, context.DeadlineExceeded)
		}
		if err := c.timing.Sleep(ctx, hint); err != nil {
			return errors.Join(last, err)
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

// timing defaults to real time. Tests can supply a shared clock and sleep before
// using a client or limiter, including when fake HTTP servers run concurrently.
type timing struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (t timing) Now() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
func (t timing) Sleep(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	return wait(ctx, d)
}

// Limiter is usable as a zero value and safe for concurrent callers. Writes to
// distinct spaces are independent; reactions share an identity-wide 5/s gate.
// No burst is allowed. Canceled waiters do not consume future capacity.
type Limiter struct {
	mu        sync.Mutex
	timing    timing
	spaces    map[string]*pace
	reactions *pace
}
type pace struct {
	gate chan struct{}
	next time.Time
}

func (l *Limiter) WaitWrite(ctx context.Context, space string) error {
	return l.acquire(ctx, space, false)
}
func (l *Limiter) WaitReaction(ctx context.Context) error { return l.acquire(ctx, "", true) }

func (l *Limiter) acquire(ctx context.Context, space string, reaction bool) error {
	l.mu.Lock()
	if l.spaces == nil {
		l.spaces = make(map[string]*pace)
	}
	p := l.spaces[space]
	if reaction {
		p = l.reactions
	}
	if p == nil {
		p = &pace{gate: make(chan struct{}, 1)}
		if reaction {
			l.reactions = p
		} else {
			l.spaces[space] = p
		}
	}
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.gate <- struct{}{}:
	}
	defer func() { <-p.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := p.next.Sub(l.timing.Now()); delay > 0 {
		if err := l.timing.Sleep(ctx, delay); err != nil {
			return err
		}
	}
	interval := time.Second
	if reaction {
		interval = time.Second / 5
	}
	p.next = l.timing.Now().Add(interval)
	return nil
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
