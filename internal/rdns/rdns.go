// Package rdns holds what the Hetzner Robot and Hetzner Cloud reverse DNS
// clients have in common: the errors the controller acts on, and Multi, which
// routes each IP to the client of the account it belongs to.
package rdns

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrIPNotFound means the IP does not belong to the account.
	ErrIPNotFound = errors.New("ip not found")
	// ErrRateLimited means the API rejected the request because of its rate limit.
	ErrRateLimited = errors.New("rate limit exceeded")

	errNotOwned = fmt.Errorf("%w in any configured account", ErrIPNotFound)
)

// Client reads and writes the PTR record of an IP. It returns an error matching
// ErrIPNotFound for IPs outside its account.
type Client interface {
	GetPTR(ctx context.Context, ip string) (string, error)
	SetPTR(ctx context.Context, ip, ptr string) error
	DeletePTR(ctx context.Context, ip string) error
}

// Multi is a Client over several accounts. It finds the account an IP belongs
// to by reading the IP from each client in turn, and remembers it.
type Multi struct {
	clients []Client

	mu     sync.Mutex
	owners map[string]Client // keyed by IP
}

// NewMulti returns a Multi over clients, asked in the given order.
func NewMulti(clients ...Client) *Multi {
	return &Multi{clients: clients, owners: map[string]Client{}}
}

// GetPTR reads ip from the client it belongs to. If that client no longer
// knows ip, the other clients are asked again.
func (m *Multi) GetPTR(ctx context.Context, ip string) (string, error) {
	if owner := m.owner(ip); owner != nil {
		ptr, err := owner.GetPTR(ctx, ip)
		if !errors.Is(err, ErrIPNotFound) {
			return ptr, err
		}
		m.setOwner(ip, nil)
	}

	// A failing client must not block the IPs of the others, so its error is
	// only returned if no client knows ip.
	var firstErr error
	for _, c := range m.clients {
		ptr, err := c.GetPTR(ctx, ip)
		if err == nil {
			m.setOwner(ip, c)
			return ptr, nil
		}
		if firstErr == nil && !errors.Is(err, ErrIPNotFound) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", errNotOwned
}

// SetPTR sets the PTR of ip in the account it belongs to.
func (m *Multi) SetPTR(ctx context.Context, ip, ptr string) error {
	return m.write(ctx, ip, func(c Client) error { return c.SetPTR(ctx, ip, ptr) })
}

// DeletePTR deletes the PTR of ip in the account it belongs to.
func (m *Multi) DeletePTR(ctx context.Context, ip string) error {
	return m.write(ctx, ip, func(c Client) error { return c.DeletePTR(ctx, ip) })
}

func (m *Multi) write(ctx context.Context, ip string, fn func(Client) error) error {
	owner := m.owner(ip)
	if owner == nil {
		if _, err := m.GetPTR(ctx, ip); err != nil {
			return err
		}
		owner = m.owner(ip)
	}
	err := fn(owner)
	if errors.Is(err, ErrIPNotFound) {
		m.setOwner(ip, nil)
	}
	return err
}

func (m *Multi) owner(ip string) Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owners[ip]
}

func (m *Multi) setOwner(ip string, c Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c == nil {
		delete(m.owners, ip)
		return
	}
	m.owners[ip] = c
}
