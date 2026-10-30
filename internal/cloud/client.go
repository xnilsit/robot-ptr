// Package cloud reads and writes the reverse DNS of Hetzner Cloud servers
// through their Primary IPs
// (https://docs.hetzner.cloud/reference/cloud).
package cloud

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/xnilsit/robot-ptr/internal/rdns"
)

// actionTimeout bounds the wait for a PTR change, which blocks a reconcile
// worker.
const actionTimeout = 30 * time.Second

// Client manages the PTR records of the Primary IPs of one Hetzner Cloud
// project. A server's public IPs are always Primary IPs, so the IP alone
// identifies the record: an IPv4 Primary IP is the address itself, an IPv6 one
// the /64 network containing it.
type Client struct {
	api *hcloud.Client

	mu  sync.Mutex
	ids map[string]int64 // Primary IP ID keyed by IP
}

// NewClient returns a Client for the project of token.
func NewClient(token string, opts ...hcloud.ClientOption) *Client {
	opts = append([]hcloud.ClientOption{
		hcloud.WithToken(token),
		// The controller requeues failed reconciles, and backs off on
		// rdns.ErrRateLimited. Retrying here instead would block the reconcile
		// and hide the rate limit.
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
	}, opts...)
	return &Client{
		api: hcloud.NewClient(opts...),
		ids: map[string]int64{},
	}
}

// GetPTR returns the PTR record for ip, which is Hetzner's generated name or
// "" if none was set.
func (c *Client) GetPTR(ctx context.Context, ip string) (string, error) {
	addr, err := parseIP(ip)
	if err != nil {
		return "", err
	}
	pip, err := c.lookup(ctx, ip, addr)
	if err != nil {
		return "", err
	}
	for k, ptr := range pip.DNSPtr {
		if net.ParseIP(k).Equal(addr) {
			return ptr, nil
		}
	}
	return "", nil
}

// SetPTR creates or updates the PTR record for ip.
func (c *Client) SetPTR(ctx context.Context, ip, ptr string) error {
	return c.change(ctx, ip, &ptr)
}

// DeletePTR resets the PTR record for ip to Hetzner's generated name.
func (c *Client) DeletePTR(ctx context.Context, ip string) error {
	return c.change(ctx, ip, nil)
}

// Account names the Cloud project in messages.
func (c *Client) Account() string { return "the Cloud project" }

// IsServer reports whether providerID is the one hcloud-cloud-controller-manager
// sets on Hetzner Cloud servers. hcloud://bm- belongs to Robot servers
// registered by older versions.
func (c *Client) IsServer(providerID string) bool {
	return strings.HasPrefix(providerID, "hcloud://") && !strings.HasPrefix(providerID, "hcloud://bm-")
}

// lookup returns the Primary IP containing addr, by its remembered ID if
// possible.
func (c *Client) lookup(ctx context.Context, ip string, addr net.IP) (*hcloud.PrimaryIP, error) {
	if id := c.id(ip); id != 0 {
		pip, _, err := c.api.PrimaryIP.GetByID(ctx, id)
		if err != nil {
			return nil, mapError(err)
		}
		if pip != nil && contains(pip, addr) {
			return pip, nil
		}
		// The Primary IP was deleted, or its ID now holds another IP.
		c.setID(ip, 0)
	}

	pip, err := c.search(ctx, addr)
	if err != nil {
		return nil, mapError(err)
	}
	if pip == nil {
		return nil, fmt.Errorf("%s is not a primary ip of the hcloud project: %w", ip, rdns.ErrIPNotFound)
	}
	c.setID(ip, pip.ID)
	return pip, nil
}

// search finds the Primary IP containing addr. The ip filter of the API only
// matches whole Primary IPs, so IPv6 addresses are looked up in the list of
// all Primary IPs.
func (c *Client) search(ctx context.Context, addr net.IP) (*hcloud.PrimaryIP, error) {
	if addr.To4() != nil {
		pip, _, err := c.api.PrimaryIP.GetByIP(ctx, addr.String())
		return pip, err
	}
	pips, err := c.api.PrimaryIP.All(ctx)
	if err != nil {
		return nil, err
	}
	for _, pip := range pips {
		if contains(pip, addr) {
			return pip, nil
		}
	}
	return nil, nil
}

func (c *Client) change(ctx context.Context, ip string, ptr *string) error {
	addr, err := parseIP(ip)
	if err != nil {
		return err
	}
	id := c.id(ip)
	cached := id != 0
	if !cached {
		pip, err := c.lookup(ctx, ip, addr)
		if err != nil {
			return err
		}
		id = pip.ID
	}

	action, _, err := c.api.RDNS.ChangeDNSPtr(ctx, &hcloud.PrimaryIP{ID: id}, addr, ptr)
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		// The Primary IP may have been deleted, or recreated with a new ID.
		c.setID(ip, 0)
		if cached {
			pip, lookupErr := c.lookup(ctx, ip, addr)
			if lookupErr != nil {
				return lookupErr
			}
			action, _, err = c.api.RDNS.ChangeDNSPtr(ctx, pip, addr, ptr)
			if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
				c.setID(ip, 0)
			}
		}
	}
	if err != nil {
		return mapError(err)
	}
	// The change is applied asynchronously; waiting surfaces its errors.
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	if err := c.api.Action.WaitFor(ctx, action); err != nil {
		return fmt.Errorf("change_dns_ptr action: %w", mapError(err))
	}
	return nil
}

func parseIP(ip string) (net.IP, error) {
	addr := net.ParseIP(ip)
	if addr == nil {
		return nil, fmt.Errorf("invalid ip %q", ip)
	}
	return addr, nil
}

// contains reports whether addr is pip, or for IPv6 lies in its network.
func contains(pip *hcloud.PrimaryIP, addr net.IP) bool {
	if pip.Network != nil {
		return pip.Network.Contains(addr)
	}
	return pip.IP.Equal(addr)
}

// mapError wraps a rate limit error with rdns.ErrRateLimited. A not_found
// error is left alone: it may come from a stale ID or a vanished action, while
// rdns.ErrIPNotFound is reserved for an IP that no Primary IP contains.
func mapError(err error) error {
	if hcloud.IsError(err, hcloud.ErrorCodeRateLimitExceeded) {
		return fmt.Errorf("%w: %w", rdns.ErrRateLimited, err)
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
