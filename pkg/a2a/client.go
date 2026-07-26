// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package a2a implements the v0.1 synchronous A2A client from
// docs/a2a-design.md ("Mast as A2A client", client-only phasing row):
// agent-card discovery and caching, JSON-RPC 2.0 message/send on the
// single A2A v0.3 endpoint (A2A-Version header, bearer auth from an
// env-var reference), direct-message and task-opened reply handling
// with bounded tasks/get polling to a terminal state, and tasks/cancel
// on caller cancellation. Static agent configs load from
// .agents/a2a/*.yaml (see AgentConfig / LoadDir; wired into pkg/config
// root scanning).
//
// Build-vs-reuse note (docs/a2a-design.md v0.1 phasing row asks that
// ADK v2.1.0's agentregistry package be evaluated before hand-building
// client machinery): evaluated 2026-07-26 and declined for v0.1. That
// package is a Google Cloud Agent Registry client — ADC-authenticated
// discovery plus RemoteAgent factories over github.com/a2aproject/
// a2a-go/v2, aimed at the registry-discovery story that is explicitly
// v0.2 here. Using its factories for the static-config path would add
// a2a-go/v2 as a direct dependency and an ADK RemoteAgent abstraction
// on top, to obtain three JSON-RPC methods and a card GET that fit in
// this file against stdlib net/http. Revisit at v0.2, where streaming
// (message/stream over SSE) and registry discovery make the SDK earn
// its place.
//
// Layering note: Send returns *federation.Result and wraps the
// federation sentinel errors directly rather than defining a parallel
// error/result vocabulary — pkg/federation is protocol-neutral and
// does not import this package, so the dependency is one-way.
package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-steer/mast/pkg/federation"
)

// Client is a synchronous A2A v0.3 client for one configured agent.
// Safe for concurrent use.
type Client struct {
	cfg  AgentConfig
	http *http.Client

	// pollInterval paces the tasks/get loop (tests shrink it).
	pollInterval time.Duration

	nextID atomic.Int64

	// card cache: fetched at most once per process lifetime. Refresh
	// cadence (SIGHUP / TTL / CLI) is docs/a2a-design.md open question
	// 2 and lands with v0.2 config hot-reload.
	cardMu   sync.Mutex
	card     *AgentCard
	endpoint string
}

// ClientOption customizes a Client.
type ClientOption func(*Client)

// WithHTTPClient substitutes the transport (tests, custom TLS/proxies).
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.http = h }
}

// WithPollInterval overrides the tasks/get polling cadence.
func WithPollInterval(d time.Duration) ClientOption {
	return func(c *Client) { c.pollInterval = d }
}

// NewClient validates cfg and returns a Client. No network I/O happens
// until the first call (card fetch is lazy).
func NewClient(cfg AgentConfig, opts ...ClientOption) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		cfg:          cfg,
		http:         &http.Client{},
		pollInterval: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Name returns the configured agent name.
func (c *Client) Name() string { return c.cfg.Name }

// Card returns the agent card, fetching and caching it on first use.
// Endpoint-only configs (no agent_card_url) return (nil, nil): the
// card is optional when the operator pinned the endpoint directly.
func (c *Client) Card(ctx context.Context) (*AgentCard, error) {
	c.cardMu.Lock()
	defer c.cardMu.Unlock()
	return c.cardLocked(ctx)
}

func (c *Client) cardLocked(ctx context.Context) (*AgentCard, error) {
	if c.card != nil {
		return c.card, nil
	}
	if c.cfg.AgentCardURL == "" {
		return nil, nil
	}
	cardURL, err := resolveCardURL(c.cfg.AgentCardURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: build card request: %w", c.cfg.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(VersionHeader, ProtocolVersion)
	if err := c.setAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: fetch agent card %s: %v", federation.ErrUnreachable, c.cfg.Name, cardURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s: agent card fetch returned HTTP %d", federation.ErrAuthFailed, c.cfg.Name, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: %s: agent card fetch %s returned HTTP %d", c.cfg.Name, cardURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: read agent card: %w", c.cfg.Name, err)
	}
	card := &AgentCard{}
	if err := json.Unmarshal(body, card); err != nil {
		return nil, fmt.Errorf("a2a: %s: parse agent card: %w", c.cfg.Name, err)
	}
	c.card = card
	return card, nil
}

// resolveCardURL appends the well-known path to bare base URLs.
func resolveCardURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("a2a: agent_card_url %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = WellKnownCardPath
	}
	return u.String(), nil
}

// endpointURL resolves the JSON-RPC endpoint: explicit config endpoint
// first, else the card's JSON-RPC interface. A card advertising only
// non-JSONRPC transports is a protocol mismatch (docs/a2a-design.md:
// JSON-RPC only at first; gRPC/REST alternates declined).
func (c *Client) endpointURL(ctx context.Context) (string, error) {
	c.cardMu.Lock()
	defer c.cardMu.Unlock()
	if c.endpoint != "" {
		return c.endpoint, nil
	}
	if c.cfg.Endpoint != "" {
		c.endpoint = c.cfg.Endpoint
		return c.endpoint, nil
	}
	card, err := c.cardLocked(ctx)
	if err != nil {
		return "", err
	}
	// Absent preferredTransport defaults to JSON-RPC per spec.
	if card.PreferredTransport == "" || card.PreferredTransport == TransportJSONRPC {
		if card.URL == "" {
			return "", fmt.Errorf("a2a: %s: agent card has no url", c.cfg.Name)
		}
		c.endpoint = card.URL
		return c.endpoint, nil
	}
	for _, iface := range card.AdditionalInterfaces {
		if iface.Transport == TransportJSONRPC && iface.URL != "" {
			c.endpoint = iface.URL
			return c.endpoint, nil
		}
	}
	return "", fmt.Errorf("%w: %s: agent card offers no JSONRPC interface (preferredTransport=%q; v0.1 speaks JSON-RPC only)",
		federation.ErrProtocolMismatch, c.cfg.Name, card.PreferredTransport)
}

// setAuth attaches the bearer token resolved from the configured env
// var. Resolution happens per request so token rotation needs no
// restart. A configured-but-unset env var is an auth failure, caught
// before any bytes leave the process.
func (c *Client) setAuth(req *http.Request) error {
	if c.cfg.Auth == nil {
		return nil
	}
	token := os.Getenv(c.cfg.Auth.TokenEnv)
	if token == "" {
		return fmt.Errorf("%w: %s: auth.token_env %s is not set in the environment", federation.ErrAuthFailed, c.cfg.Name, c.cfg.Auth.TokenEnv)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// Send invokes skill on the remote agent with inputs and blocks until
// a terminal state or the bounded timeout (opts precedence: timeout
// argument > config timeout_seconds > DefaultTimeout). The reply may
// be a direct message (returned as a completed Result) or an opened
// task, which Send polls via tasks/get. If the caller's ctx is
// canceled or the bound expires mid-task, Send issues tasks/cancel
// (best effort, on a detached context) before returning.
//
// Skill selection: A2A v0.3 message/send has no first-class skill
// selector — AgentSkill is card-level metadata and MessageSendParams
// carries only the message. Mast conveys the chosen skill as message
// metadata under "skillId" (and validates it against the fetched card
// and the config's skills allowlist). Servers that route by content
// ignore the hint harmlessly.
func (c *Client) Send(ctx context.Context, skill string, inputs map[string]any, timeout time.Duration) (*federation.Result, error) {
	if timeout <= 0 {
		timeout = c.cfg.Timeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := c.checkSkill(ctx, skill); err != nil {
		return nil, err
	}
	endpoint, err := c.endpointURL(ctx)
	if err != nil {
		return nil, err
	}

	msg := &Message{
		Kind:      "message",
		MessageID: newID(),
		Role:      "user",
		Parts:     []Part{{Kind: "data", Data: inputs}},
	}
	if skill != "" {
		msg.Metadata = map[string]any{"skillId": skill}
	}
	blocking := true
	raw, err := c.call(ctx, endpoint, methodMessageSend, messageSendParams{
		Message:       msg,
		Configuration: &messageSendConfig{Blocking: &blocking},
	})
	if err != nil {
		return nil, err
	}
	reply, err := decodeSendReply(raw)
	if err != nil {
		return nil, err
	}

	// Direct message reply: terminal by construction.
	if reply.Message != nil {
		res := resultFromParts(reply.Message.Parts, nil)
		res.State = string(TaskStateCompleted)
		res.Raw = reply.Raw
		return res, nil
	}

	// Task reply: poll to terminal within the remaining budget.
	task := reply.Task
	for !task.Status.State.Terminal() {
		if task.Status.State == TaskStateInputRequired || task.Status.State == TaskStateAuthRequired {
			// Remote HITL propagation needs programmatic pause — v0.2
			// (docs/a2a-design.md call lifecycle; durable-execution
			// phasing). Cancel rather than burn the timeout budget.
			c.cancelTask(ctx, endpoint, task.ID)
			return nil, fmt.Errorf("%w: %s: task %s entered %q, which v0.1's synchronous client cannot service (HITL propagation is v0.2); task canceled",
				federation.ErrRemoteFailed, c.cfg.Name, task.ID, task.Status.State)
		}
		select {
		case <-ctx.Done():
			c.cancelTask(ctx, endpoint, task.ID)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %s: task %s still %q after %s; tasks/cancel sent",
					federation.ErrTimeout, c.cfg.Name, task.ID, task.Status.State, timeout)
			}
			return nil, fmt.Errorf("%s: task %s canceled by caller; tasks/cancel sent: %w", c.cfg.Name, task.ID, context.Cause(ctx))
		case <-time.After(c.pollInterval):
		}
		raw, err := c.call(ctx, endpoint, methodTasksGet, taskQueryParams{ID: task.ID})
		if err != nil {
			// A transport error racing ctx expiry: still try to cancel.
			if ctx.Err() != nil {
				c.cancelTask(ctx, endpoint, task.ID)
			}
			return nil, err
		}
		polled := &Task{}
		if err := json.Unmarshal(raw, polled); err != nil {
			return nil, fmt.Errorf("a2a: %s: malformed tasks/get result: %w", c.cfg.Name, err)
		}
		polled.Kind = "task"
		task = polled
		reply.Raw = raw
	}

	switch task.Status.State {
	case TaskStateCompleted:
		var statusParts []Part
		if task.Status.Message != nil {
			statusParts = task.Status.Message.Parts
		}
		res := resultFromParts(statusParts, task.Artifacts)
		res.State = string(task.Status.State)
		res.RemoteID = task.ID
		res.Raw = reply.Raw
		return res, nil
	default: // failed, canceled, rejected
		detail := ""
		if task.Status.Message != nil {
			if txt := textOf(task.Status.Message.Parts); txt != "" {
				detail = ": " + txt
			}
		}
		return nil, fmt.Errorf("%w: %s: task %s ended %q%s", federation.ErrRemoteFailed, c.cfg.Name, task.ID, task.Status.State, detail)
	}
}

// checkSkill enforces the config allowlist and, when a card is
// available, validates the skill id against it. Card-less
// (endpoint-only) configs skip card validation.
func (c *Client) checkSkill(ctx context.Context, skill string) error {
	if skill == "" {
		return nil
	}
	if len(c.cfg.Skills) > 0 {
		allowed := false
		for _, s := range c.cfg.Skills {
			if s == skill {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("a2a: %s: skill %q is not in the configured allowlist %v (%s)", c.cfg.Name, skill, c.cfg.Skills, c.cfg.Filename)
		}
	}
	card, err := c.Card(ctx)
	if err != nil || card == nil {
		return err
	}
	for _, s := range card.Skills {
		if s.ID == skill {
			return nil
		}
	}
	ids := make([]string, 0, len(card.Skills))
	for _, s := range card.Skills {
		ids = append(ids, s.ID)
	}
	return fmt.Errorf("a2a: %s: skill %q not present in agent card (card skills: %s)", c.cfg.Name, skill, strings.Join(ids, ", "))
}

// call performs one JSON-RPC 2.0 request against the single endpoint.
func (c *Client) call(ctx context.Context, endpoint, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: marshal %s: %w", c.cfg.Name, method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: build %s request: %w", c.cfg.Name, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(VersionHeader, ProtocolVersion)
	if err := c.setAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: %s: %s: %v", federation.ErrTimeout, c.cfg.Name, method, err)
		case errors.Is(err, context.Canceled):
			// Caller cancellation, not a network fault: preserve the
			// context error in the chain for errors.Is.
			return nil, fmt.Errorf("a2a: %s: %s: %w", c.cfg.Name, method, err)
		default:
			return nil, fmt.Errorf("%w: %s: %s: %v", federation.ErrUnreachable, c.cfg.Name, method, err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s: %s returned HTTP %d", federation.ErrAuthFailed, c.cfg.Name, method, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: %s: %s returned HTTP %d", c.cfg.Name, method, resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: read %s response: %w", c.cfg.Name, method, err)
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("a2a: %s: parse %s response: %w", c.cfg.Name, method, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("a2a: %s: %s: %w", c.cfg.Name, method, rpcResp.Error)
	}
	if len(rpcResp.Result) == 0 {
		return nil, fmt.Errorf("a2a: %s: %s response has neither result nor error", c.cfg.Name, method)
	}
	return rpcResp.Result, nil
}

// cancelTask issues tasks/cancel best-effort on a detached context —
// the triggering ctx is typically already expired or canceled.
func (c *Client) cancelTask(ctx context.Context, endpoint, taskID string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = c.call(dctx, endpoint, methodTasksCancel, taskIDParams{ID: taskID})
}

// resultFromParts folds message parts and artifact parts into a
// federation.Result: text parts concatenate (artifact parts first,
// then status-message parts, matching "artifacts are the output;
// status message is commentary"), data parts merge with later keys
// winning.
func resultFromParts(statusParts []Part, artifacts []Artifact) *federation.Result {
	res := &federation.Result{}
	var texts []string
	merge := func(parts []Part) {
		for _, p := range parts {
			switch p.Kind {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "data":
				if len(p.Data) > 0 {
					if res.Output == nil {
						res.Output = map[string]any{}
					}
					for k, v := range p.Data {
						res.Output[k] = v
					}
				}
			}
		}
	}
	for _, a := range artifacts {
		merge(a.Parts)
	}
	merge(statusParts)
	res.Text = strings.Join(texts, "\n")
	return res
}

func textOf(parts []Part) string {
	var texts []string
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// newID returns a random 128-bit hex id for messageId. The spec asks
// for client-generated unique ids; hex-random avoids a uuid dep.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("a2a: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}
