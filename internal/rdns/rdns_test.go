package rdns

import (
	"context"
	"errors"
	"testing"
)

type fakeClient struct {
	ptrs  map[string]string // IPs of the account
	err   error             // returned by every call if set
	calls int
}

func (f *fakeClient) GetPTR(_ context.Context, ip string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	ptr, ok := f.ptrs[ip]
	if !ok {
		return "", ErrIPNotFound
	}
	return ptr, nil
}

func (f *fakeClient) SetPTR(_ context.Context, ip, ptr string) error {
	f.calls++
	if _, ok := f.ptrs[ip]; !ok {
		return ErrIPNotFound
	}
	f.ptrs[ip] = ptr
	return nil
}

func (f *fakeClient) DeletePTR(ctx context.Context, ip string) error {
	return f.SetPTR(ctx, ip, "")
}

func TestMultiRoutesToOwner(t *testing.T) {
	ctx := context.Background()
	robot := &fakeClient{ptrs: map[string]string{"192.0.2.1": "robot.example.com"}}
	cloud := &fakeClient{ptrs: map[string]string{"198.51.100.1": "cloud.example.com"}}
	m := NewMulti(robot, cloud)

	if ptr, err := m.GetPTR(ctx, "198.51.100.1"); err != nil || ptr != "cloud.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	if robot.calls != 1 || cloud.calls != 1 {
		t.Fatalf("calls robot=%d cloud=%d", robot.calls, cloud.calls)
	}

	// The owner is remembered, so the Robot account is not asked again.
	if err := m.SetPTR(ctx, "198.51.100.1", "mail.example.com"); err != nil {
		t.Fatal(err)
	}
	if ptr, err := m.GetPTR(ctx, "198.51.100.1"); err != nil || ptr != "mail.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	if robot.calls != 1 || cloud.calls != 3 {
		t.Fatalf("calls robot=%d cloud=%d", robot.calls, cloud.calls)
	}
}

func TestMultiWriteDiscoversOwner(t *testing.T) {
	cloud := &fakeClient{ptrs: map[string]string{"198.51.100.1": ""}}
	m := NewMulti(&fakeClient{ptrs: map[string]string{}}, cloud)
	if err := m.SetPTR(context.Background(), "198.51.100.1", "mail.example.com"); err != nil {
		t.Fatal(err)
	}
	if cloud.ptrs["198.51.100.1"] != "mail.example.com" {
		t.Fatalf("ptr not set: %v", cloud.ptrs)
	}
}

func TestMultiNotFound(t *testing.T) {
	m := NewMulti(&fakeClient{ptrs: map[string]string{}}, &fakeClient{ptrs: map[string]string{}})
	if _, err := m.GetPTR(context.Background(), "203.0.113.1"); !errors.Is(err, ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := m.DeletePTR(context.Background(), "203.0.113.1"); !errors.Is(err, ErrIPNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestMultiFailingClientDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	down := errors.New("connection refused")
	robot := &fakeClient{err: down}
	cloud := &fakeClient{ptrs: map[string]string{"198.51.100.1": "cloud.example.com"}}
	m := NewMulti(robot, cloud)

	if ptr, err := m.GetPTR(ctx, "198.51.100.1"); err != nil || ptr != "cloud.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
	// An IP no client knows reports the failure rather than ErrIPNotFound,
	// since the failing client might own it.
	if _, err := m.GetPTR(ctx, "203.0.113.1"); !errors.Is(err, down) {
		t.Fatalf("err = %v", err)
	}
}

func TestMultiOwnerChanges(t *testing.T) {
	ctx := context.Background()
	robot := &fakeClient{ptrs: map[string]string{"192.0.2.1": "robot.example.com"}}
	cloud := &fakeClient{ptrs: map[string]string{}}
	m := NewMulti(robot, cloud)
	if _, err := m.GetPTR(ctx, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}

	// The IP moves to the Cloud project.
	delete(robot.ptrs, "192.0.2.1")
	cloud.ptrs["192.0.2.1"] = "cloud.example.com"
	if ptr, err := m.GetPTR(ctx, "192.0.2.1"); err != nil || ptr != "cloud.example.com" {
		t.Fatalf("got %q, %v", ptr, err)
	}
}
