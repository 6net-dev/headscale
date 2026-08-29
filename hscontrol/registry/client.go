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
	"time"
)

// The four short calls are bounded here as well as by their caller's context:
// register and rotate run inside the head's open write transaction, so a
// Gentility that accepts a connection and then says nothing must not hold a
// database transaction open for as long as the kernel will keep the socket.
const requestTimeout = 5 * time.Second

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
}

// New returns a client for the Gentility at baseURL, authenticating every
// call — in both directions — with bearer.
func New(baseURL, bearer string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		bearer:  bearer,
		http:    &http.Client{Timeout: requestTimeout},
		stream:  &http.Client{},
	}
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
// reconnect is exactly the window in which it may have missed one.
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
// since is always sent, because the head always has a cursor: zero is "replay
// everything the stream still holds", which is what a head with no cache
// wants, and a persisted cursor is what a head with one wants.
//
// A cursor below the compaction horizon is answered [ErrBehindHorizon] — as a
// [BehindHorizonError] carrying the horizon — before any event is delivered.
// The head clears its cursor, full-refreshes every cached World, and starts
// again.
func (c *Client) Changes(ctx context.Context, since int64, h ChangeHandler) error {
	if since < 0 {
		return fmt.Errorf("registry: %w: negative cursor %d", ErrInvalidRequest, since)
	}

	path := "/overlay/head/changes?since=" + strconv.FormatInt(since, 10)

	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")
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

	if h.Connected != nil {
		h.Connected()
	}

	return readChanges(ctx, resp.Body, h.Changed)
}

// readChanges decodes the Server-Sent Events framing Gentility writes: an
// event is "id: <seq>", "data: <json>", blank line; a heartbeat is the comment
// ": keepalive". Comments and unknown fields are ignored, per the SSE rules,
// and so is "id" — the cursor the head persists is the seq inside the payload,
// and an id line that disagreed with it would be a second answer to what the
// cursor is.
func readChanges(ctx context.Context, body io.Reader, changed func(Change)) error {
	reader := newLineReader(body)

	var data []string

	for {
		line, err := reader.line()
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
