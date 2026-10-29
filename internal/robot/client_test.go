package robot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xnilsit/robot-ptr/internal/rdns"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
			t.Errorf("missing or wrong basic auth")
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("user", "pass")
	c.BaseURL = srv.URL
	return c
}

func TestGetPTR(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rdns/1.2.3.4" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"rdns":{"ip":"1.2.3.4","ptr":"mail.example.com"}}`))
	})
	ptr, err := c.GetPTR(context.Background(), "1.2.3.4")
	if err != nil || ptr != "mail.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
}

func TestGetPTRNotFound(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"code":"RDNS_NOT_FOUND","message":"The ip address has no rdns entry"}}`))
	})
	ptr, err := c.GetPTR(context.Background(), "1.2.3.4")
	if err != nil || ptr != "" {
		t.Fatalf("got %q, %v", ptr, err)
	}
}

func TestSetPTR(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("ptr") != "mail.example.com" {
			t.Errorf("ptr = %q, %v", r.PostForm.Get("ptr"), err)
		}
		_, _ = w.Write([]byte(`{"rdns":{"ip":"1.2.3.4","ptr":"mail.example.com"}}`))
	})
	if err := c.SetPTR(context.Background(), "1.2.3.4", "mail.example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePTRNotFoundIsNoError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"code":"RDNS_NOT_FOUND","message":"not found"}}`))
	})
	if err := c.DeletePTR(context.Background(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimit(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"status":403,"code":"RATE_LIMIT_EXCEEDED","message":"Rate limit exceeded"}}`))
	})
	err := c.SetPTR(context.Background(), "1.2.3.4", "x.example.com")
	if !errors.Is(err, rdns.ErrRateLimited) {
		t.Fatalf("expected rate limit error, got %v", err)
	}
}

func TestIPNotFound(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"code":"IP_NOT_FOUND","message":"The IP address is not found"}}`))
	})
	ctx := context.Background()
	if _, err := c.GetPTR(ctx, "1.2.3.4"); !errors.Is(err, rdns.ErrIPNotFound) || IsNotFound(err) {
		t.Fatalf("GetPTR: expected ip not found error, got %v", err)
	}
	if err := c.SetPTR(ctx, "1.2.3.4", "x.example.com"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("SetPTR: expected ip not found error, got %v", err)
	}
	if err := c.DeletePTR(ctx, "1.2.3.4"); !errors.Is(err, rdns.ErrIPNotFound) {
		t.Fatalf("DeletePTR: expected ip not found error, got %v", err)
	}
}

func TestUnknownNotFoundIsError(t *testing.T) {
	// A 404 without a Robot error body, e.g. from a wrong --robot-url.
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	if _, err := c.GetPTR(context.Background(), "1.2.3.4"); err == nil {
		t.Fatal("expected error")
	}
}
