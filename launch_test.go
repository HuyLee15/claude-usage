package main

import (
	"testing"
)

func usageResult(name string, fiveHour, weekly float64, err error) result {
	return result{
		Account: &account{Name: name},
		Usage: &usage{
			FiveHour: &window{Utilization: fiveHour},
			SevenDay: &window{Utilization: weekly},
		},
		Err: err,
	}
}

func TestSelectAccountChoosesLowestWorstWindow(t *testing.T) {
	results := []result{
		usageResult("work", 20, 75, nil),
		usageResult("personal", 40, 45, nil),
		usageResult("team", 10, 95, nil),
	}
	got, err := selectAccount(results, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Name != "personal" {
		t.Fatalf("selected %q, want personal", got.Account.Name)
	}
}

func TestSelectAccountHonorsMinimumRemainingQuota(t *testing.T) {
	results := []result{
		usageResult("nearly-empty", 95, 20, nil),
		usageResult("available", 30, 40, nil),
	}
	got, err := selectAccount(results, 50, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Name != "available" {
		t.Fatalf("selected %q, want available", got.Account.Name)
	}
}

func TestSelectAccountSkipsFailedOrIncompleteResults(t *testing.T) {
	results := []result{
		usageResult("failed", 1, 1, errLoginRequired),
		{Account: &account{Name: "missing"}},
		usageResult("no-week", 1, 1, nil),
	}
	results[2].Usage.SevenDay = nil
	if _, err := selectAccount(results, 0, 0); err == nil {
		t.Fatal("expected an error when no eligible account exists")
	}
}
