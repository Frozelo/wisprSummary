package wispr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const protocolVersion = "2025-06-18"

// Client is a minimal MCP client over Streamable HTTP (JSON-RPC 2.0 via POST).
// It only issues read calls: initialize, tools/list, tools/call.
type Client struct {
	url string
	hc  *http.Client
	ts  *TokenSource

	mu      sync.Mutex
	session string
	ready   bool
	nextID  int
}

func NewClient(url string, hc *http.Client, ts *TokenSource) *Client {
	return &Client{url: url, hc: hc, ts: ts}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC error returned by the server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// Tool describes an MCP tool from tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is a tools/call result.
type ToolResult struct {
	Content           []ToolContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text joins all text blocks of the result.
func (r ToolResult) Text() string {
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var out ToolResult
	err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out)
	if err == nil && out.IsError {
		return out, fmt.Errorf("tool %s failed: %s", name, snippet([]byte(out.Text())))
	}
	return out, err
}

// call sends a request, handling lazy initialization, one token refresh on 401
// and one re-initialization if the server dropped the session.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	if err := c.ensureInit(ctx); err != nil {
		return err
	}
	err := c.roundTrip(ctx, method, params, result)
	if se, ok := errors.AsType[*statusError](err); ok {
		switch se.Code {
		case http.StatusUnauthorized:
			c.ts.Invalidate()
		case http.StatusNotFound: // session expired
			c.reset()
			if err := c.ensureInit(ctx); err != nil {
				return err
			}
		default:
			return err
		}
		return c.roundTrip(ctx, method, params, result)
	}
	return err
}

func (c *Client) reset() {
	c.mu.Lock()
	c.ready, c.session = false, ""
	c.mu.Unlock()
}

func (c *Client) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if ready {
		return nil
	}
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0.1"},
	}
	var res json.RawMessage
	err := c.roundTrip(ctx, "initialize", params, &res)
	if se, ok := errors.AsType[*statusError](err); ok && se.Code == http.StatusUnauthorized {
		c.ts.Invalidate()
		err = c.roundTrip(ctx, "initialize", params, &res)
	}
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	return nil
}

type statusError struct {
	Code int
	Body string
}

func (e *statusError) Error() string { return fmt.Sprintf("mcp http %d: %s", e.Code, e.Body) }

func (c *Client) notify(ctx context.Context, method string) error {
	resp, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) roundTrip(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	resp, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}

	var msg *rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, err = readSSE(resp.Body, id)
	} else {
		var m rpcResponse
		if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&m); err == nil {
			msg = &m
		}
	}
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	if msg.Error != nil {
		return msg.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(msg.Result, result)
}

func (c *Client) post(ctx context.Context, body rpcRequest) (*http.Response, error) {
	tok, err := c.ts.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	c.mu.Unlock()

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &statusError{Code: resp.StatusCode, Body: snippet(b)}
	}
	return resp, nil
}

// readSSE returns the JSON-RPC response with the given id from an event stream.
func readSSE(r io.Reader, id int) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var data strings.Builder
	flush := func() *rpcResponse {
		defer data.Reset()
		if data.Len() == 0 {
			return nil
		}
		var m rpcResponse
		if json.Unmarshal([]byte(data.String()), &m) != nil || m.ID == nil || *m.ID != id {
			return nil
		}
		return &m
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m := flush(); m != nil {
				return m, nil
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if m := flush(); m != nil {
		return m, nil
	}
	return nil, errors.New("no response in event stream")
}
