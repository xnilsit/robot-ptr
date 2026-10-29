// Package cloud reads and writes the reverse DNS of Hetzner Cloud servers
// through their Primary IPs
// (https://docs.hetzner.cloud/reference/cloud).
package cloud

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/xnilsit/robot-ptr/internal/rdns"
)

// Client manages the PTR records of the Primary IPs of one Hetzner Cloud
// project. A server's public IPs are always Primary IPs, so the IP alone
// identifies the record.
type Client struct {
	api *hcloud.Client

	mu  sync.Mutex
	ids map[string]int64 // Primary IP ID keyed by IP
}

// NewClient returns a Client for the project of token.
func NewClient(token string, opts ...hcloud.ClientOption) *Client {
	return &Client{
		api: hcloud.NewClient(append([]hcloud.ClientOption{hcloud.WithToken(token)}, opts...)...),
		ids: map[string]int64{},
	}
}

// GetPTR returns the PTR record for ip, which is Hetzner's generated name if
// none was set.
func (c *Client) GetPTR(ctx context.Context, ip string) (string, error) {
	pip, err := c.lookup(ctx, ip)
	if err != nil {
		return "", err
	}
	return pip.DNSPtr[ip], nil
}

// SetPTR creates or updates the PTR record for ip.
func (c *Client) SetPTR(ctx context.Context, ip, ptr string) error {
	return c.change(ctx, ip, &ptr)
}

// DeletePTR resets the PTR record for ip to Hetzner's generated name.
func (c *Client) DeletePTR(ctx context.Context, ip string) error {
	return c.change(ctx, ip, nil)
}

func (c *Client) lookup(ctx context.Context, ip string) (*hcloud.PrimaryIP, error) {
	pip, _, err := c.api.PrimaryIP.GetByIP(ctx, ip)
	if err != nil {
		return nil, mapError(err)
	}
	if pip == nil {
		c.setID(ip, 0)
		return nil, fmt.Errorf("%s is not a primary ip of the hcloud project: %w", ip, rdns.ErrIPNotFound)
	}
	c.setID(ip, pip.ID)
	return pip, nil
}

func (c *Client) change(ctx context.Context, ip string, ptr *string) error {
	addr := net.ParseIP(ip)
	if addr == nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	id := c.id(ip)
	if id == 0 {
		pip, err := c.lookup(ctx, ip)
		if err != nil {
			return err
		}
		id = pip.ID
	}

	action, _, err := c.api.RDNS.ChangeDNSPtr(ctx, &hcloud.PrimaryIP{ID: id}, addr, ptr)
	if err != nil {
		// The Primary IP may have been deleted, or recreated with a new ID.
		if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			c.setID(ip, 0)
		}
		return mapError(err)
	}
	// The change is applied asynchronously; waiting surfaces its errors.
	return mapError(c.api.Action.WaitFor(ctx, action))
}

// mapError wraps the hcloud errors the controller acts on with their rdns
// counterpart.
func mapError(err error) error {
	switch {
	case hcloud.IsError(err, hcloud.ErrorCodeRateLimitExceeded):
		return fmt.Errorf("%w: %w", rdns.ErrRateLimited, err)
	case hcloud.IsError(err, hcloud.ErrorCodeNotFound):
		return fmt.Errorf("%w: %w", rdns.ErrIPNotFound, err)
	}
	return err
}

func (c *Client) id(ip string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ids[ip]
}

func (c *Client) setID(ip string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 {
		delete(c.ids, ip)
		return
	}
	c.ids[ip] = id
}
