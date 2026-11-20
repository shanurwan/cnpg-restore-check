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

func TestParseBackupID(t *testing.T) {
	// backup_label.old from a lab restore of a plugin-barman-cloud standby backup.
	label := `START WAL LOCATION: 5/C0041F0 (file 00000001000000050000000C)
CHECKPOINT LOCATION: 5/D000060
BACKUP METHOD: streamed
BACKUP FROM: standby
START TIME: 2026-11-20 01:00:00 UTC
LABEL: Barman backup cloud 20261120T010000
START TIMELINE: 1
`
	if got := parseBackupID(label); got != "20261120T010000" {
		t.Fatalf("got %q", got)
	}
	if got := parseBackupID("LABEL: manual\n"); got != "manual" {
		t.Fatalf("other labels are kept as is, got %q", got)
	}
	if got := parseBackupID("START TIMELINE: 1\n"); got != "" {
		t.Fatalf("no label, got %q", got)
	}
}

func TestParseHistory(t *testing.T) {
	tl, lsn := parseHistory("1\t5/6D000000\tno recovery target specified\n")
	if tl != 1 || lsn != "5/6D000000" {
		t.Fatalf("got %d %q", tl, lsn)
	}
	// A source that already failed over: the last line is the timeline the
	// restore replayed.
	tl, lsn = parseHistory("1\t0/3000000\tno recovery target specified\n\n2\t0/5000160\tat restore point \"x\"\n")
	if tl != 2 || lsn != "0/5000160" {
		t.Fatalf("got %d %q", tl, lsn)
	}
	if tl, lsn := parseHistory("# comment\n"); tl != 0 || lsn != "" {
		t.Fatalf("got %d %q", tl, lsn)
	}
}
