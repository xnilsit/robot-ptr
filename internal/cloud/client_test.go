package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/xnilsit/robot-ptr/internal/rdns"
)

const primaryIP = `{"primary_ips":[{"id":42,"ip":"203.0.113.10","type":"ipv4",
	"dns_ptr":[{"ip":"203.0.113.10","dns_ptr":"mail.example.com"}]}],
	"meta":{"pagination":{"page":1,"per_page":50,"previous_page":null,"next_page":null,"last_page":1,"total_entries":1}}}`

const noPrimaryIP = `{"primary_ips":[],
	"meta":{"pagination":{"page":1,"per_page":50,"previous_page":null,"next_page":null,"last_page":1,"total_entries":0}}}`

func newTestServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing or wrong token")
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewClient("token", hcloud.WithEndpoint(srv.URL),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}))
}

func TestGetPTR(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/primary_ips" || r.URL.Query().Get("ip") != "203.0.113.10" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(primaryIP))
	})
	ptr, err := c.GetPTR(context.Background(), "203.0.113.10")
	if err != nil || ptr != "mail.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
}

func TestGetPTRNotPrimaryIP(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(noPrimaryIP))
	})
	if _, err := c.GetPTR(context.Background(), "203.0.113.10"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := c.SetPTR(context.Background(), "203.0.113.10", "mail.example.com"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetPTRCachedID(t *testing.T) {
	var lookups, gets int
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/primary_ips":
			lookups++
			_, _ = w.Write([]byte(primaryIP))
		case "/primary_ips/42":
			gets++
			_, _ = w.Write([]byte(`{"primary_ip":{"id":42,"ip":"203.0.113.10","type":"ipv4",
				"dns_ptr":[{"ip":"203.0.113.10","dns_ptr":"smtp.example.com"}]}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	for _, want := range []string{"mail.example.com", "smtp.example.com"} {
		if ptr, err := c.GetPTR(context.Background(), "203.0.113.10"); err != nil || ptr != want {
			t.Fatalf("got %q, %v, want %q", ptr, err, want)
		}
	}
	if lookups != 1 || gets != 1 {
		t.Fatalf("lookups = %d, gets = %d", lookups, gets)
	}
}

func TestGetPTRCachedIDReassigned(t *testing.T) {
	var lookups int
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/primary_ips":
			lookups++
			_, _ = w.Write([]byte(primaryIP))
		case "/primary_ips/42":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"primary_ip with ID '42' not found"}}`))
		}
	})
	c.setID("203.0.113.10", 42)
	if ptr, err := c.GetPTR(context.Background(), "203.0.113.10"); err != nil || ptr != "mail.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d", lookups)
	}
}

func TestGetPTRIPv6(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/primary_ips" || r.URL.Query().Has("ip") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(`{"primary_ips":[
			{"id":42,"ip":"203.0.113.10","type":"ipv4","dns_ptr":[]},
			{"id":43,"ip":"2001:db8:1:2::/64","type":"ipv6",
				"dns_ptr":[{"ip":"2001:db8:1:2::1","dns_ptr":"mail.example.com"}]}],
			"meta":{"pagination":{"page":1,"per_page":50,"previous_page":null,"next_page":null,"last_page":1,"total_entries":2}}}`))
	})
	ctx := context.Background()
	if ptr, err := c.GetPTR(ctx, "2001:db8:1:2:0:0:0:1"); err != nil || ptr != "mail.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	if ptr, err := c.GetPTR(ctx, "2001:db8:1:2::2"); err != nil || ptr != "" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	if _, err := c.GetPTR(ctx, "2001:db8:1:3::1"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimited(t *testing.T) {
	var requests int
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"limit of 3600 requests per hour reached"}}`))
	})
	if _, err := c.GetPTR(context.Background(), "203.0.113.10"); !errors.Is(err, rdns.ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
	// hcloud-go does not retry, so the controller sees the rate limit at once.
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestIsServer(t *testing.T) {
	c := NewClient("token")
	for id, want := range map[string]bool{"hcloud://123": true, "hcloud://bm-123": false, "hrobot://123": false, "": false} {
		if got := c.IsServer(id); got != want {
			t.Errorf("IsServer(%q) = %v", id, got)
		}
	}
}

func TestChangePTR(t *testing.T) {
	for name, tc := range map[string]struct {
		call func(*Client) error
		want *string
	}{
		"set":    {func(c *Client) error { return c.SetPTR(context.Background(), "203.0.113.10", "smtp.example.com") }, new("smtp.example.com")},
		"delete": {func(c *Client) error { return c.DeletePTR(context.Background(), "203.0.113.10") }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var lookups, polls int
			var body struct {
				IP     string  `json:"ip"`
				DNSPtr *string `json:"dns_ptr"`
			}
			c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/primary_ips":
					lookups++
					_, _ = w.Write([]byte(primaryIP))
				case r.Method == http.MethodPost && r.URL.Path == "/primary_ips/42/actions/change_dns_ptr":
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					_, _ = w.Write([]byte(`{"action":{"id":7,"command":"change_dns_ptr","status":"running","progress":0}}`))
				case r.Method == http.MethodGet && r.URL.Path == "/actions":
					polls++
					_, _ = w.Write([]byte(`{"actions":[{"id":7,"command":"change_dns_ptr","status":"success","progress":100}],
						"meta":{"pagination":{"page":1,"per_page":50,"previous_page":null,"next_page":null,"last_page":1,"total_entries":1}}}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
			})

			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if body.IP != "203.0.113.10" || (body.DNSPtr == nil) != (tc.want == nil) ||
				(tc.want != nil && *body.DNSPtr != *tc.want) {
				t.Fatalf("unexpected request body %+v", body)
			}
			if polls != 1 {
				t.Fatalf("action polled %d times", polls)
			}

			// The Primary IP ID is remembered.
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if lookups != 1 {
				t.Fatalf("lookups = %d", lookups)
			}
		})
	}
}

func TestChangePTRActionFails(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/primary_ips":
			_, _ = w.Write([]byte(primaryIP))
		case "/primary_ips/42/actions/change_dns_ptr":
			_, _ = w.Write([]byte(`{"action":{"id":7,"command":"change_dns_ptr","status":"error","progress":100,
				"error":{"code":"invalid_input","message":"invalid dns_ptr"}}}`))
		}
	})
	err := c.SetPTR(context.Background(), "203.0.113.10", "-.example.com")
	if err == nil || !strings.Contains(err.Error(), "invalid dns_ptr") {
		t.Fatalf("err = %v", err)
	}
}

func TestChangePTRPrimaryIPGone(t *testing.T) {
	gone := false
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/primary_ips" && gone:
			_, _ = w.Write([]byte(noPrimaryIP))
		case r.URL.Path == "/primary_ips":
			_, _ = w.Write([]byte(primaryIP))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"primary_ip with ID '42' not found"}}`))
		}
	})
	ctx := context.Background()
	if _, err := c.GetPTR(ctx, "203.0.113.10"); err != nil {
		t.Fatal(err)
	}
	gone = true
	if err := c.SetPTR(ctx, "203.0.113.10", "mail.example.com"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
	if c.id("203.0.113.10") != 0 {
		t.Fatal("stale primary ip id kept")
	}
}
