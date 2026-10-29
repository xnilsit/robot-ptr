// Package robot is a minimal client for the reverse DNS endpoints of the
// Hetzner Robot webservice (https://robot.hetzner.com/doc/webservice/en.html#reverse-dns).
package robot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xnilsit/robot-ptr/internal/rdns"
)

// DefaultBaseURL is the Hetzner Robot webservice endpoint.
const DefaultBaseURL = "https://robot-ws.your-server.de"

// Client talks to the Robot webservice using HTTP basic auth.
type Client struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

// NewClient returns a Client with sane defaults.
func NewClient(username, password string) *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		Username:   username,
		Password:   password,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is an error response returned by the Robot webservice.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("robot api: %d %s: %s", e.Status, e.Code, e.Message)
}

// Is makes errors.Is match the IP_NOT_FOUND and RATE_LIMIT_EXCEEDED errors
// against rdns.ErrIPNotFound and rdns.ErrRateLimited.
func (e *APIError) Is(target error) bool {
	switch target {
	case rdns.ErrIPNotFound:
		return e.Code == "IP_NOT_FOUND"
	case rdns.ErrRateLimited:
		return e.Code == "RATE_LIMIT_EXCEEDED"
	}
	return false
}

// IsNotFound reports whether err means the IP has no reverse DNS entry. It is
// false for IPs that do not belong to the account, see IsIPNotFound.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "RDNS_NOT_FOUND"
}

// IsIPNotFound reports whether err means the IP does not belong to the Robot
// account, e.g. because it is not a Hetzner Robot server.
func IsIPNotFound(err error) bool {
	return errors.Is(err, rdns.ErrIPNotFound)
}

// IsRateLimited reports whether err is a Robot rate limit response.
func IsRateLimited(err error) bool {
	return errors.Is(err, rdns.ErrRateLimited)
}

type rdnsEnvelope struct {
	RDNS struct {
		IP  string `json:"ip"`
		PTR string `json:"ptr"`
	} `json:"rdns"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// GetPTR returns the PTR record for ip. It returns "" and no error if the IP
// has no reverse DNS entry.
func (c *Client) GetPTR(ctx context.Context, ip string) (string, error) {
	var out rdnsEnvelope
	err := c.do(ctx, http.MethodGet, ip, nil, &out)
	if IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return out.RDNS.PTR, nil
}

// SetPTR creates or updates the PTR record for ip.
func (c *Client) SetPTR(ctx context.Context, ip, ptr string) error {
	return c.do(ctx, http.MethodPost, ip, url.Values{"ptr": {ptr}}, nil)
}

// DeletePTR removes the PTR record for ip. Deleting a non-existent entry is
// not an error.
func (c *Client) DeletePTR(ctx context.Context, ip string) error {
	err := c.do(ctx, http.MethodDelete, ip, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) do(ctx context.Context, method, ip string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/rdns/" + url.PathEscape(ip)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		var e errorEnvelope
		if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
			return &e.Error
		}
		return &APIError{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(data))}
	}

	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
