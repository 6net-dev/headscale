// Package registry speaks the head ↔ Gentility channel, v1: the four calls of
// docs/plans/2026-08-28-overlay-phase1-master.md §2.3, over public HTTPS with
// a shared bearer.
//
// It lives in the fork rather than in a shared module because the World
// contract belongs to whoever ships both ends of it: evolving a client in
// another repository would mean cutting a release of that repository for every
// change to a document this repository is the only consumer of.
//
// # The status codes are the contract
//
// The head is a program, not a browser: it cannot be shown a page, so every
// refusal says on the wire what it means and what the head should do next.
// Each call below maps its own column of §2.3's table to a typed error,
// because the same status means different things on different calls — a 404 is
// an unknown node on world and rotate but an unknown pre-auth key on register,
// a 409 is "not rendered yet, wait" on world and "this contradicts the record,
// stop" on the two callbacks, and a 410 is a spent pre-auth key on register but
// a cursor below the compaction horizon on changes.
package registry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The four short calls are bounded here as well as by their caller's context:
// register and rotate run inside the head's open write transaction, so a
// Gentility that accepts a connection and then says nothing must not hold a
// database transaction open for as long as the kernel will keep the socket.
const requestTimeout = 5 * time.Second

// The change stream is bounded by evidence rather than by a deadline, in two
// places, because a connection that is open is not a stream that is working
// and nothing in TCP distinguishes the two.
const (
	// responseHeaderTimeout bounds the wait for Gentility's status line.
	// There is no stream to watch yet, so the transport is what holds this
	// one: a peer that accepts the connection and then says nothing would
	// otherwise be waited on for as long as the kernel keeps the socket.
	responseHeaderTimeout = 10 * time.Second

	// defaultIdleTimeout is how long an OPEN stream may go without a line
	// before it is torn down for the reconnect to replace. Gentility
	// heartbeats, so silence is not a quiet network — it is a stream that
	// has stopped, and a head that keeps waiting on one is a head that has
	// stopped hearing about changes without knowing it.
	defaultIdleTimeout = 60 * time.Second
)

// Errors, one per cell of §2.3's status table that a caller can act on.
var (
	// ErrUnauthorized: 401. The bearer is not the one Gentility expects.
	ErrUnauthorized = errors.New("registry: not the head")

	// ErrRateLimited: 429.
	ErrRateLimited = errors.New("registry: too many calls")

	// ErrUnknownNode: 404 on world and rotate. Gentility has never heard of
	// this head node id, or has forgotten it — a revoked node is refused as
	// unknown rather than served the last World it held.
	ErrUnknownNode = errors.New("registry: unknown node")

	// ErrUnknownKey: 404 on register. No such pre-auth key here.
	ErrUnknownKey = errors.New("registry: unknown pre-auth key")

	// ErrNotRendered: 409 on world. The node exists and nothing has rendered
	// it yet. The only refusal here the head waits out: the change stream is
	// about to say the document exists.
	ErrNotRendered = errors.New("registry: node not rendered yet")

	// ErrRefused: 409 on register and rotate. The write contradicts what is
	// recorded — a revocation, a head node id that moved, a rotation computed
	// against a key the node has moved on from. A human looks at this; nothing
	// retries it.
	ErrRefused = errors.New("registry: refused")

	// ErrExpired: 410 on register. The pre-auth key is past its ten minutes.
	ErrExpired = errors.New("registry: pre-auth key expired")

	// ErrCompromised: 403 on register. A different machine presented itself
	// for a pre-auth key that is already bound.
	ErrCompromised = errors.New("registry: enrollment compromised")

	// ErrInvalidRequest: 422. A body or a cursor missing something the
	// contract names. Both ends are shipped together, so this is a bug report
	// rather than a user error.
	ErrInvalidRequest = errors.New("registry: invalid request")

	// ErrStreamIdle: no line arrived on an open change stream inside the
	// idle window. Not a refusal — Gentility never answered anything — so
	// the head reconnects with the cursor it has, exactly as it would after
	// any other drop.
	ErrStreamIdle = errors.New("registry: change stream went idle")

	// ErrBehindHorizon: 410 on changes. Match with errors.Is; recover the
	// horizon itself with errors.As on [BehindHorizonError].
	ErrBehindHorizon = errors.New("registry: cursor behind the compaction horizon")
)

// BehindHorizonError is the 410 the change stream answers a cursor below the
// compaction horizon with. It carries the horizon so the head can say how far
// it fell behind before it clears its cursor and full-refreshes.
type BehindHorizonError struct {
	CompactionHorizon int64
}

func (e *BehindHorizonError) Error() string {
	return fmt.Sprintf(
		"registry: cursor behind the compaction horizon (%d)",
		e.CompactionHorizon,
	)
}

// Is makes errors.Is(err, ErrBehindHorizon) true for this error, so a caller
// that does not need the horizon can match the sentinel like any other.
func (e *BehindHorizonError) Is(target error) bool { return target == ErrBehindHorizon }

// Client is one head's connection to Gentility. Safe for concurrent use.
type Client struct {
	baseURL string
	bearer  string

	// http bounds the four short calls; stream deliberately does not bound
	// the change stream, whose whole purpose is to stay open. A Timeout on
	// an http.Client covers the WHOLE response including the body, so a
	// timeout-bearing client would cut the stream at its deadline no matter
	// how much traffic was flowing through it.
	http   *http.Client
	stream *http.Client

	// idleTimeout is how long the open stream may go without a line. See
	// [defaultIdleTimeout].
	idleTimeout time.Duration
}

// Option configures a [Client] at construction. Everything the contract fixes
// is a constant; an Option is for what a deployment may legitimately differ
// on.
type Option func(*Client)

// WithIdleTimeout sets how long an open change stream may go without a line
// before [Client.Changes] tears it down with [ErrStreamIdle]. It must be
// comfortably longer than Gentility's heartbeat interval, or a healthy stream
// is cut every heartbeat gap.
func WithIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.idleTimeout = d }
}

// New returns a client for the Gentility at baseURL, authenticating every
// call — in both directions — with bearer.
func New(baseURL, bearer string, opts ...Option) *Client {
	// Cloned rather than built, so the stream keeps the proxy, dialer and
	// pool settings the default transport carries and differs only where
	// this channel needs it to.
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the stdlib's own type
	transport.ResponseHeaderTimeout = responseHeaderTimeout

	client := &Client{
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		bearer:      bearer,
		http:        &http.Client{Timeout: requestTimeout},
		stream:      &http.Client{Transport: transport},
		idleTimeout: defaultIdleTimeout,
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

// World fetches the current World for one head node id.
//
// The id is the head's own stable node id, not the node key: a key rotates and
// a World keyed by one would be a document that moved when nothing about the
// node did.
func (c *Client) World(ctx context.Context, headNodeID string) (*World, error) {
	req, err := c.request(ctx, http.MethodGet, "/overlay/head/world/"+url.PathEscape(headNodeID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: world request: %w", err)
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var world World
		if err := json.NewDecoder(resp.Body).Decode(&world); err != nil {
			return nil, fmt.Errorf("registry: decoding world: %w", err)
		}

		return &world, nil
	case http.StatusNotFound:
		return nil, ErrUnknownNode
	case http.StatusConflict:
		return nil, ErrNotRendered
	default:
		return nil, c.common(resp, "world")
	}
}

// Register binds a pre-auth key to the node the head has just allocated.
//
// Synchronous by contract: the head calls this from inside its own write
// transaction and rolls that transaction back on any error, so a registration
// is never committed on the head that Gentility has not recorded.
func (c *Client) Register(ctx context.Context, reg Registration) (*RegistrationResult, error) {
	resp, err := c.post(ctx, "/overlay/head/register", reg)
	if err != nil {
		return nil, err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var result RegistrationResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("registry: decoding registration: %w", err)
		}

		return &result, nil
	case http.StatusNotFound:
		return nil, ErrUnknownKey
	case http.StatusGone:
		return nil, ErrExpired
	case http.StatusForbidden:
		return nil, ErrCompromised
	case http.StatusConflict:
		return nil, ErrRefused
	default:
		return nil, c.common(resp, "register")
	}
}

// Rotate records a node key rotation, synchronously and inside the head's
// transaction for the same reason [Client.Register] is. Idempotent on the new
// key: a retry with the same rotation is a 204 like the first call.
func (c *Client) Rotate(ctx context.Context, rot Rotation) error {
	resp, err := c.post(ctx, "/overlay/head/rotate", rot)
	if err != nil {
		return err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrUnknownNode
	case http.StatusConflict:
		return ErrRefused
	default:
		return c.common(resp, "rotate")
	}
}

// ChangeHandler is what a caller does with the stream.
//
// Connected is called once each time the stream opens, BEFORE any event —
// that is the head's cue to refresh every World it has cached, since a
// reconnect is exactly the window in which it may have missed one. It is what
// makes a nil cursor safe: the refresh, not the backlog, is how a head with
// no cursor catches up.
// Changed is called for each event, in seq order.
//
// Both are called on the goroutine that called [Client.Changes], so neither
// needs a lock of its own and neither may block for long: a slow handler is
// backpressure on the stream.
type ChangeHandler struct {
	Connected func()
	Changed   func(Change)
}

// Changes follows the change stream from a cursor until the context is
// cancelled, the deadline Gentility sets on its own response passes, or the
// connection drops. A clean end of stream is not an error: the caller
// reconnects with the cursor it has.
//
// # A nil cursor is the tail, and it is not since=0
//
// since is a POINTER because "the cursor I persisted" and "I have no cursor"
// are different requests, and no number can say the second. A nil since omits
// the parameter and Gentility serves from now on; a non-nil one, zero
// included, replays from that seq.
//
// A head with no persisted cursor — a cold start, a lost cache file — wants
// the tail. It has cached no World, so there is no backlog whose effect it
// missed, and its Connected handler full-refreshes every World it does hold
// before a single event is served. Replaying the stream's whole history into
// that would be work whose answer the refresh already has.
//
// # Recovery from the horizon asks for the tail too
//
// A cursor below the compaction horizon is answered [ErrBehindHorizon] — as a
// [BehindHorizonError] carrying the horizon — before any event is delivered.
// The head then full-refreshes every cached World and reconnects with a NIL
// cursor. Not since=0: every seq below the horizon is gone, so zero is a point
// Gentility has already compacted away and it earns the same 410 again,
// forever. The refresh is what brings the head level; the tail is what keeps
// it there.
func (c *Client) Changes(ctx context.Context, since *int64, h ChangeHandler) error {
	if since != nil && *since < 0 {
		return fmt.Errorf("registry: %w: negative cursor %d", ErrInvalidRequest, *since)
	}

	path := "/overlay/head/changes"
	if since != nil {
		path += "?since=" + strconv.FormatInt(*since, 10)
	}

	// The stream's own context, so the idle watchdog has something to
	// cancel: the read below is blocked in the kernel, and closing the body
	// under it is the only thing that wakes it.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := c.request(streamCtx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	// Both types are named because this call really does accept both. The
	// stream is what a 200 is made of; every refusal on it is JSON, and one
	// of them carries a value the head acts on — the 410's
	// compaction_horizon. An Accept naming only text/event-stream would be
	// claiming not to accept the answer we parse in [behindHorizon].
	//
	// It is not a workaround for a 406. Gentility puts this endpoint behind
	// `plug :accepts, ["json"]`, and whether text/event-stream negotiates
	// through that depends on the MIME types compiled into that
	// application — a build-time fact of another repository, which is
	// exactly the sort of thing this header should not be silently relying
	// on either way.
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.stream.Do(req)
	if err != nil {
		return fmt.Errorf("registry: change stream: %w", err)
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusGone:
		return behindHorizon(resp)
	default:
		return c.common(resp, "changes")
	}

	// Before the watchdog, not after: Connected full-refreshes every cached
	// World, and no line can arrive while it runs, so a slow refresh would
	// otherwise trip a stream that has done nothing wrong.
	if h.Connected != nil {
		h.Connected()
	}

	var idle atomic.Bool

	watchdog := time.AfterFunc(c.idleTimeout, func() {
		idle.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	err = readChanges(streamCtx, resp.Body, h.Changed, func() {
		watchdog.Reset(c.idleTimeout)
	})

	// The cancellation the watchdog caused surfaces as a context error;
	// name what really happened, since the two call for the same reconnect
	// but read very differently in a log.
	if err != nil && idle.Load() {
		return fmt.Errorf("%w after %s", ErrStreamIdle, c.idleTimeout)
	}

	return err
}

// readChanges decodes the Server-Sent Events framing Gentility writes, calling
// alive for every line it reads: an
// event is "id: <seq>", "data: <json>", blank line; a heartbeat is the comment
// ": keepalive". Comments and unknown fields are ignored, per the SSE rules,
// and so is "id" — the cursor the head persists is the seq inside the payload,
// and an id line that disagreed with it would be a second answer to what the
// cursor is.
func readChanges(ctx context.Context, body io.Reader, changed func(Change), alive func()) error {
	reader := newLineReader(body)

	var data []string

	for {
		line, err := reader.line()
		if err == nil {
			// Any line at all, heartbeat included: the stream is
			// alive, which is the whole of what the watchdog asks.
			alive()
		}

		if err != nil {
			// A cancelled context surfaces here as a read error on a body
			// that was closed under us. Report the cancellation, which is
			// what the caller can act on.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}

			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("registry: reading change stream: %w", err)
		}

		switch {
		case line == "":
			// End of an event. A frame with no data field is discarded.
			if len(data) == 0 {
				continue
			}

			var change Change
			if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &change); err != nil {
				return fmt.Errorf("registry: decoding change event: %w", err)
			}

			data = data[:0]

			if changed != nil {
				changed(change)
			}
		case strings.HasPrefix(line, ":"):
			// A comment. ": keepalive" is the only one Gentility sends.
		default:
			field, value := splitField(line)
			if field == "data" {
				data = append(data, value)
			}
		}
	}
}

// splitField splits one SSE line into its field name and value: everything
// before the first colon, and everything after it less one optional leading
// space. A line with no colon is a field with an empty value.
func splitField(line string) (string, string) {
	field, value, found := strings.Cut(line, ":")
	if !found {
		return line, ""
	}

	return field, strings.TrimPrefix(value, " ")
}

// behindHorizon reads the horizon out of the change stream's 410 so the head
// can report how far behind it fell. A body that does not carry one is still
// a 410: the status is the decision and the number is only the detail.
func behindHorizon(resp *http.Response) error {
	var payload struct {
		CompactionHorizon int64 `json:"compaction_horizon"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&payload)

	return &BehindHorizonError{CompactionHorizon: payload.CompactionHorizon}
}

func (c *Client) post(ctx context.Context, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("registry: encoding %s body: %w", path, err)
	}

	req, err := c.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %s request: %w", path, err)
	}

	return resp, nil
}

func (c *Client) request(
	ctx context.Context,
	method, path string,
	body io.Reader,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("registry: building %s request: %w", path, err)
	}

	// Both directions of this channel carry the same shared secret (§2.3).
	req.Header.Set("Authorization", "Bearer "+c.bearer)

	return req, nil
}

// common maps the statuses every call answers the same way, and names the call
// in anything left over so an unexpected status is traceable to one endpoint.
func (c *Client) common(resp *http.Response, call string) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusUnprocessableEntity:
		return ErrInvalidRequest
	default:
		return fmt.Errorf("registry: %s returned %d", call, resp.StatusCode)
	}
}

// drain reads and closes a response body so the connection returns to the
// pool rather than being torn down after every call.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

// Gentility's own frames are three short lines; anything past this is not a
// frame this stream could have issued, and reading it would be an unbounded
// allocation driven by whatever is on the other end of the socket.
const maxLineBytes = 1 << 20

// lineReader splits a stream into SSE lines. The three terminators the format
// allows — CRLF, LF and a bare CR — are all accepted, which is why this is not
// bufio.Scanner: ScanLines knows nothing about a lone CR.
type lineReader struct {
	reader *bufio.Reader
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{reader: bufio.NewReader(r)}
}

// line returns the next line without its terminator. An unterminated final
// line is discarded rather than returned, per the SSE rules: a frame that was
// cut off mid-write is not a frame.
func (lr *lineReader) line() (string, error) {
	var out strings.Builder

	for {
		char, err := lr.reader.ReadByte()
		if err != nil {
			return "", err
		}

		switch char {
		case '\n':
			return out.String(), nil
		case '\r':
			// CRLF counts once: swallow the LF when it is the next byte.
			if next, err := lr.reader.ReadByte(); err == nil && next != '\n' {
				_ = lr.reader.UnreadByte()
			}

			return out.String(), nil
		default:
			if out.Len() >= maxLineBytes {
				return "", fmt.Errorf("registry: change stream line over %d bytes", maxLineBytes)
			}

			out.WriteByte(char)
		}
	}
}
