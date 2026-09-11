package main

import (
	"bytes"
	"context"
	"testing"
)

func TestHelpExitsSuccessfully(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &bytes.Buffer{}, &output, &output); code != 0 {
		t.Fatalf("run(--help) returned %d, output:\n%s", code, output.String())
	}
}

func TestDeniedAccountStopsBeforeLoadingAWSCredentials(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), []string{
		"--account", "123456789012",
		"--region", "ap-south-1",
		"--services", "rds",
		"--resource-id", "database-1",
		"--deny-account", "123456789012",
	}, &bytes.Buffer{}, &output, &output)
	if code != 1 {
		t.Fatalf("run() returned %d, output:\n%s", code, output.String())
	}
}

func TestParseOptionsRequiresExplicitSelector(t *testing.T) {
	_, err := parseOptions([]string{
		"--account", "123456789012",
		"--region", "ap-south-1",
		"--services", "rds",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseOptions accepted a command without a selector")
	}
}

func TestParseOptionsReadsServicesTagsAndDenylist(t *testing.T) {
	t.Setenv("AWS_NUKE_DENY_ACCOUNTS", "999999999999, 888888888888")
	options, err := parseOptions([]string{
		"--account", "123456789012",
		"--region", "ap-south-1",
		"--services", "rds,msk",
		"--tag", "Environment=preview",
		"--tag", "Owner=payments",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.serviceNames) != 2 || options.serviceNames[0] != "rds" || options.serviceNames[1] != "msk" {
		t.Fatalf("service names = %v", options.serviceNames)
	}
	if options.tags["Environment"] != "preview" || options.tags["Owner"] != "payments" {
		t.Fatalf("tags = %v", options.tags)
	}
	if len(options.deniedAccounts) != 2 || !contains(options.deniedAccounts, "888888888888") {
		t.Fatalf("denied accounts = %v", options.deniedAccounts)
	}
}

func TestConfirmRequiresExactAccount(t *testing.T) {
	var output bytes.Buffer
	if err := confirm(bytes.NewBufferString("123456789013\n"), &output, "123456789012", ""); err == nil {
		t.Fatal("confirm accepted the wrong account")
	}
	if err := confirm(bytes.NewBufferString("123456789012\n"), &output, "123456789012", ""); err != nil {
		t.Fatalf("confirm rejected the correct account: %v", err)
	}
	if err := confirm(&bytes.Buffer{}, &output, "123456789012", "123456789013"); err == nil {
		t.Fatal("confirm accepted a mismatched --confirm-account")
	}
}
