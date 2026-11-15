package checks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
)

func TestConnectRetriesUntilTheServiceRoutes(t *testing.T) {
	attempts := 0
	connect := func(context.Context, Target, string) (Conn, error) {
		attempts++
		return nil, errors.New("connection refused")
	}
	start := time.Now()
	_, err := connectWithRetry(context.Background(), connect, Target{}, 2500*time.Millisecond)
	if err == nil {
		t.Fatal("want the last error after the retry window")
	}
	if attempts < 2 {
		t.Fatalf("want retries, got %d attempt(s)", attempts)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("retry window overran: %s", time.Since(start))
	}
}

func TestConnectStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	connect := func(context.Context, Target, string) (Conn, error) { return nil, errors.New("refused") }
	start := time.Now()
	if _, err := connectWithRetry(ctx, connect, Target{}, time.Minute); err == nil {
		t.Fatal("want an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("did not stop on context cancel: %s", time.Since(start))
	}
}

func TestRunStopsAfterFailedConnect(t *testing.T) {
	old := ConnectRetryFor
	ConnectRetryFor = 0
	defer func() { ConnectRetryFor = old }()
	connect := func(context.Context, Target, string) (Conn, error) { return nil, errors.New("refused") }
	res := Run(context.Background(), connect, Target{}, v1alpha1.ChecksSpec{
		Amcheck: &v1alpha1.AmcheckSpec{},
		SQL:     []v1alpha1.SQLCheck{{Name: "x", Query: "select true"}},
	}, time.Now())
	if res.Passed() || len(res.Checks) != 1 || res.Checks[0].Name != "connect" {
		t.Fatalf("want only a failed connect check, got %+v", res.Checks)
	}
}
