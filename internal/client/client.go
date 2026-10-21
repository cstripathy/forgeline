// Package client is a small Go client for the Forgeline server API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/cstripathy/forgeline/internal/server"
)

// Client talks to one Forgeline server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for the server at baseURL, such as http://127.0.0.1:8080.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token,
		HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// Error is a non-2xx response from the server.
type Error struct {
	Status   int
	Message  string
	Problems []server.ProblemJSON
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("server returned %d: %s", e.Status, e.Message)
	for _, p := range e.Problems {
		msg += fmt.Sprintf("\n  %s: %s", p.Path, p.Msg)
	}
	return msg
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
		var body server.ErrorJSON
		if json.Unmarshal(b, &body) == nil && body.Error != "" {
			e.Message, e.Problems = body.Error, body.Problems
		}
		return nil, e
	}
	return b, nil
}

func (c *Client) run(ctx context.Context, method, path string, body io.Reader, contentType string) (server.RunJSON, error) {
	b, err := c.do(ctx, method, path, body, contentType)
	if err != nil {
		return server.RunJSON{}, err
	}
	var run server.RunJSON
	if err := json.Unmarshal(b, &run); err != nil {
		return server.RunJSON{}, fmt.Errorf("decoding response: %w", err)
	}
	return run, nil
}

// Submit queues a run. source is an optional tar.gz of the workspace (see server.PackDir).
func (c *Client) Submit(ctx context.Context, pipelineYAML, source []byte) (server.RunJSON, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormField("pipeline")
	fw.Write(pipelineYAML)
	if source != nil {
		fw, _ := mw.CreateFormFile("source", "source.tar.gz")
		fw.Write(source)
	}
	if err := mw.Close(); err != nil {
		return server.RunJSON{}, err
	}
	return c.run(ctx, "POST", "/v1/runs", &buf, mw.FormDataContentType())
}

// Get returns a run with its steps.
func (c *Client) Get(ctx context.Context, id string) (server.RunJSON, error) {
	return c.run(ctx, "GET", "/v1/runs/"+id, nil, "")
}

// Cancel asks the server to cancel a run.
func (c *Client) Cancel(ctx context.Context, id string) (server.RunJSON, error) {
	return c.run(ctx, "POST", "/v1/runs/"+id+"/cancel", nil, "")
}

// Logs returns a step's stored log.
func (c *Client) Logs(ctx context.Context, id, step string) (string, error) {
	b, err := c.do(ctx, "GET", "/v1/runs/"+id+"/steps/"+step+"/logs", nil, "")
	return string(b), err
}

// Terminal reports whether a run status is final.
func Terminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled"
}
