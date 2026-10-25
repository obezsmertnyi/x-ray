package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Version is the SDK's own version, sent to the server as the client version.
const Version = "2.0.0"

// Client is a connection to an X-Ray MCP endpoint. It is safe for concurrent
// use.
type Client struct {
	c *client.Client
}

type config struct {
	headers    map[string]string
	httpClient *http.Client
	timeout    time.Duration
	name       string
	version    string
}

// Option configures Connect.
type Option func(*config)

// WithToken sends the bearer token on every request: the shared MCP secret, or
// an OAuth access token.
func WithToken(token string) Option {
	return func(c *config) { c.headers["Authorization"] = "Bearer " + strings.TrimSpace(token) }
}

// WithHeader sends an extra header on every request.
func WithHeader(key, value string) Option {
	return func(c *config) { c.headers[key] = value }
}

// WithHTTPClient uses hc for every request, for a proxy, custom TLS roots or a
// test transport. Its Timeout still applies.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.httpClient = hc }
}

// WithTimeout bounds each HTTP request. The default is 30 seconds.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithClientInfo names the client to the server, for its logs.
func WithClientInfo(name, version string) Option {
	return func(c *config) { c.name, c.version = name, version }
}

// Connect opens a session with the MCP endpoint at url, for example
// http://localhost:8083/mcp. The caller closes the Client.
func Connect(ctx context.Context, url string, opts ...Option) (*Client, error) {
	cfg := &config{
		headers: map[string]string{},
		timeout: 30 * time.Second,
		name:    "xray-go-sdk",
		version: Version,
	}
	for _, o := range opts {
		o(cfg)
	}
	topts := []transport.StreamableHTTPCOption{transport.WithHTTPTimeout(cfg.timeout)}
	if len(cfg.headers) > 0 {
		topts = append(topts, transport.WithHTTPHeaders(cfg.headers))
	}
	if cfg.httpClient != nil {
		topts = append(topts, transport.WithHTTPBasicClient(cfg.httpClient))
	}
	c, err := client.NewStreamableHttpClient(url, topts...)
	if err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("xray: start: %w", err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: cfg.name, Version: cfg.version}
	if _, err := c.Initialize(ctx, req); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("xray: initialize: %w", err)
	}
	return &Client{c: c}, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.c.Close() }

// Tools lists the names of the tools the server offers.
func (c *Client) Tools(ctx context.Context) ([]string, error) {
	res, err := c.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("xray: list tools: %w", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

// ToolError is a failure the server reported for a call: a refused write, a
// missing item, an unknown kind. Transport failures are returned as ordinary
// errors.
type ToolError struct {
	Tool    string
	Message string
}

func (e *ToolError) Error() string { return "xray: " + e.Tool + ": " + e.Message }

// IsNotFound reports whether err is a ToolError for an item that does not exist
// or that the caller may not see.
func IsNotFound(err error) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Message == "not found"
}

// Call invokes any tool by name and returns its JSON reply. The typed methods
// are built on it; use it for a tool added after this SDK was written.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	var req mcp.CallToolRequest
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := c.c.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("xray: %s: %w", tool, err)
	}
	var text strings.Builder
	for _, part := range res.Content {
		if tc, ok := mcp.AsTextContent(part); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return nil, &ToolError{Tool: tool, Message: strings.TrimSpace(text.String())}
	}
	raw := json.RawMessage(strings.TrimSpace(text.String()))
	// A tool reports a refusal as an object with an "error" key.
	if bytes.HasPrefix(raw, []byte("{")) {
		var probe struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Error != "" {
			msg := probe.Error
			if probe.Reason != "" {
				msg += ": " + probe.Reason
			}
			return nil, &ToolError{Tool: tool, Message: msg}
		}
	}
	return raw, nil
}

// call runs a tool and decodes its reply into out.
func (c *Client) call(ctx context.Context, tool string, args map[string]any, out any) error {
	raw, err := c.Call(ctx, tool, args)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("xray: %s: decode reply: %w", tool, err)
	}
	return nil
}

// put sets args[key] = v unless v is the zero value.
func put(args map[string]any, key string, v any) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case int:
		if x == 0 {
			return
		}
	case []string:
		if len(x) == 0 {
			return
		}
	case map[string]string:
		if len(x) == 0 {
			return
		}
	}
	args[key] = v
}
