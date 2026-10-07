package certs

import (
	"context"
	"crypto"
	"errors"
	"sync/atomic"
	"testing"
)

func TestWaitForRecords(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Name: "_acme-challenge.tryparrot.site", Value: "apex"},
		{Name: "_acme-challenge.tryparrot.site", Value: "wildcard"},
	}

	var calls atomic.Int32

	// The first check sees one record, the second sees both, as if the user added them one by one.
	lookup := func(_ context.Context, name string) ([]string, error) {
		if name != "_acme-challenge.tryparrot.site" {
			return nil, errors.New("unexpected name " + name)
		}

		if calls.Add(1) <= 2 {
			return []string{"apex"}, nil
		}

		return []string{"wildcard", "apex", "stale"}, nil
	}

	var reported []int

	if err := waitForRecords(t.Context(), lookup, records, func(missing int) { reported = append(reported, missing) }); err != nil {
		t.Fatalf("waitForRecords() error: %v", err)
	}

	if len(reported) != 1 || reported[0] != 1 {
		t.Errorf("waiting reported %v, want [1]", reported)
	}
}

func TestWaitForRecordsCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	lookup := func(context.Context, string) ([]string, error) { return nil, nil }

	err := waitForRecords(ctx, lookup, []Record{{Name: "x", Value: "y"}}, func(int) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitForRecords() error = %v, want context.Canceled", err)
	}
}

func TestAccountKeyReused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	key, err := accountKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	again, err := accountKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !again.Public().(interface{ Equal(x crypto.PublicKey) bool }).Equal(key.Public()) {
		t.Error("accountKey() created a new key instead of reusing the stored one")
	}
}
