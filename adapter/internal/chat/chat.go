// Package chat wraps the authenticated Google Chat API with bounded retries,
// write pacing, and filesystem boundaries for media transfers.
package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gchat-adapter/internal/config"
	"golang.org/x/sys/unix"
	googlechat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type Space = googlechat.Space

// Client belongs to one authenticated identity. Do not reuse it after switching
// credentials: its reaction ownership and rate limits belong to that identity.
type Client struct {
	api            *googlechat.Service
	limits         Limiter
	ep             config.Endpoints
	identityMu     sync.RWMutex
	identityUserID string
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
	if !strings.HasPrefix(id, "users/") || len(strings.Split(id, "/")) != 2 || strings.TrimPrefix(id, "users/") == "" || id == "users/me" || id == "users/app" {
		return localError{errors.New("chat: canonical identity user ID required")}
	}
	c.identityMu.Lock()
	defer c.identityMu.Unlock()
	if c.identityUserID != "" && c.identityUserID != id {
		return localError{errors.New("chat: identity user ID changed; rebuild the client")}
	}
	c.identityUserID = id
	return nil
}

func (c *Client) ListSpaces(ctx context.Context) ([]Space, error) {
	var spaces []Space
	call := c.api.Spaces.List().PageSize(1000)
	for {
		var result *googlechat.ListSpacesResponse
		err := retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
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
	err = retry(ctx, requestID != "", func(ctx context.Context) error {
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
	call := c.api.Spaces.Messages.List(space).PageSize(1000).OrderBy("create_time asc").Filter(strings.Join(filters, " AND "))
	for {
		var result *googlechat.ListMessagesResponse
		err := retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
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

func (c *Client) GetMessage(ctx context.Context, name string) (*googlechat.Message, error) {
	var result *googlechat.Message
	err := retry(ctx, true, func(ctx context.Context) error {
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
		err := retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
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

func (c *Client) CreateReaction(ctx context.Context, message, emoji string) (string, error) {
	var result *googlechat.Reaction
	// Reaction creates have no request ID. Only confirmed quota rejections are
	// retried, never an ambiguous transport failure after the server accepted it.
	err := retry(ctx, false, func(ctx context.Context) error {
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
		if err := c.SetIdentityUserID(result.User.Name); err != nil {
			return result.Name, err
		}
	}
	return result.Name, nil
}

func (c *Client) DeleteReaction(ctx context.Context, message, emoji string) error {
	c.identityMu.RLock()
	user := c.identityUserID
	c.identityMu.RUnlock()
	if user == "" {
		return localError{errors.New("chat: SetIdentityUserID is required before deleting reactions")}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	call := c.api.Spaces.Messages.Reactions.List(message).PageSize(1000)
	// Collect before deleting: deleting while paging can shift page boundaries.
	var names []string
	for {
		var result *googlechat.ListReactionsResponse
		err := retry(ctx, true, func(ctx context.Context) error { var err error; result, err = call.Context(ctx).Do(); return err })
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
		err := retry(ctx, true, func(ctx context.Context) error {
			if err := c.limits.WaitReaction(ctx); err != nil {
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
	file, parent, err := openMediaFile(c.ep.UploadRoot, path, unix.O_RDONLY)
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
	err = retry(ctx, false, func(ctx context.Context) error {
		if err := c.limits.WaitWrite(ctx, space); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return localError{err}
		}
		var err error
		result, err = c.api.Media.Upload(space, &googlechat.UploadAttachmentRequest{Filename: filepath.Base(path)}).Media(&uploadReader{reader: file, remaining: limit}).Context(ctx).Do()
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
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n > 0 {
			return 0, localError{errors.New("file exceeds media limit")}
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
// directories and only uses dest after success. No path component may be a link.
func (c *Client) DownloadMedia(ctx context.Context, resourceName, dest string, maxBytes int64) (n int64, err error) {
	if maxBytes <= 0 {
		return 0, localError{errors.New("chat: positive download byte limit required")}
	}
	if c.ep.MediaMaxBytes > 0 && maxBytes > c.ep.MediaMaxBytes {
		maxBytes = c.ep.MediaMaxBytes
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var response *http.Response
	// The response body outlives retry's per-call context. Keep the download's
	// own bounded context alive until the body has been read and closed.
	err = retry(ctx, true, func(_ context.Context) error {
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
	file, parent, err := openMediaFile(c.ep.MediaDir, dest, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
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
			err = errors.Join(err, unix.Unlinkat(int(parent.Fd()), filepath.Base(dest), 0))
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

// openMediaFile walks with directory descriptors and O_NOFOLLOW, so a concurrent
// symlink replacement cannot redirect a checked path. Roots must be absolute,
// existing real directories; callers keep both returned descriptors alive.
func openMediaFile(root, path string, flags int) (*os.File, *os.File, error) {
	if root == "" {
		return nil, nil, errors.New("media root is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil, errors.New("media path is outside its configured root")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, nil, openErr
		}
		fd = next
	}
	parent := os.NewFile(uintptr(fd), filepath.Dir(path))
	fileFD, err := unix.Openat(fd, filepath.Base(path), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fileFD), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("media path is not a regular file")
	}
	if err != nil {
		file.Close()
		parent.Close()
		return nil, nil, err
	}
	return file, parent, nil
}

type localError struct{ error }

func (e localError) Unwrap() error { return e.error }

// Classify maps errors to gc receipt kinds. Retry-After accepts both delay
// seconds and an HTTP date; malformed or past hints produce a zero delay.
func Classify(err error) (kind string, retryAfter time.Duration) {
	if err == nil {
		return "", 0
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		hint := apiErr.Header.Get("Retry-After")
		if seconds, e := strconv.ParseInt(hint, 10, 64); e == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			retryAfter = time.Duration(seconds) * time.Second
		} else if when, e := http.ParseTime(hint); e == nil && time.Until(when) > 0 {
			retryAfter = time.Until(when)
		}
		switch {
		case apiErr.Code == 429:
			return "rate_limited", retryAfter
		case apiErr.Code == 403 && strings.Contains(strings.ToLower(apiErr.Message), "not a member") && strings.Contains(strings.ToLower(apiErr.Message), "space"):
			return "not_found", retryAfter
		case apiErr.Code == 401 || apiErr.Code == 403:
			return "auth", retryAfter
		case apiErr.Code == 404:
			return "not_found", retryAfter
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

// retry bounds pacing, I/O and backoff together. Only safe operations retry
// ambiguous failures. A deadline preserves the last API error and retry hint.
func retry(ctx context.Context, safe bool, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	backoff := time.Second
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		err := call(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(last, err, ctx.Err())
		}
		kind, hint := Classify(err)
		if kind != "rate_limited" && !(kind == "transient" && safe) {
			return err
		}
		last = err
		if hint < backoff {
			hint = backoff
		}
		if err := wait(ctx, hint); err != nil {
			return errors.Join(last, err)
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

// Limiter is usable as a zero value and safe for concurrent callers. Writes to
// distinct spaces are independent; reactions share an identity-wide 5/s gate.
// No burst is allowed. Canceled waiters do not consume future capacity.
type Limiter struct {
	mu        sync.Mutex
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
	if delay := time.Until(p.next); delay > 0 {
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
	interval := time.Second
	if reaction {
		interval = time.Second / 5
	}
	p.next = time.Now().Add(interval)
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
