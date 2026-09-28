package main

import (
	"context"
	"testing"
	"time"

	"Max-hack/internal/catalog"
)

type subscriptionCall struct {
	url         string
	secret      string
	updateTypes []string
}

type recordingSubscriber struct {
	calls chan subscriptionCall
}

func (s *recordingSubscriber) Subscribe(_ context.Context, url, secret string, updateTypes []string) error {
	s.calls <- subscriptionCall{url: url, secret: secret, updateTypes: append([]string(nil), updateTypes...)}
	return nil
}

func TestValidWebhookURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want bool
	}{
		{url: "https://bot.example/webhook", want: true},
		{url: "https://bot.example:443/webhook", want: true},
		{url: "http://bot.example/webhook", want: false},
		{url: "https://bot.example:8443/webhook", want: false},
		{url: "https://bot.example/other", want: false},
		{url: "https://bot.example/webhook/", want: false},
		{url: "https://bot.example/webhook?source=other", want: false},
		{url: "https://bot.example/webhook#fragment", want: false},
		{url: "https:///webhook", want: false},
		{url: "https://user:pass@bot.example/webhook", want: false},
		{url: "not a URL", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			if got := validWebhookURL(tt.url); got != tt.want {
				t.Fatalf("validWebhookURL(%q) = %t, want %t", tt.url, got, tt.want)
			}
		})
	}
}

func TestWebhookSecretPattern(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"abc_1", "ABC-123", "a2345"} {
		if !webhookSecretPattern.MatchString(value) {
			t.Errorf("expected valid webhook secret %q", value)
		}
	}
	for _, value := range []string{"", "abcd", "has space", "кириллица", "abcde!"} {
		if webhookSecretPattern.MatchString(value) {
			t.Errorf("expected invalid webhook secret %q", value)
		}
	}
}

func TestCatalogPolicyFailsClosed(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "unknown", " pilot-ish "} {
		if got := catalogPolicy(value); got != catalog.ReviewPolicyPilot {
			t.Errorf("catalogPolicy(%q) = %q, want %q", value, got, catalog.ReviewPolicyPilot)
		}
	}
	for _, want := range []catalog.ReviewPolicy{
		catalog.ReviewPolicyAll,
		catalog.ReviewPolicyCurrent,
		catalog.ReviewPolicyPilot,
	} {
		if got := catalogPolicy("  " + string(want) + "  "); got != want {
			t.Errorf("catalogPolicy(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestWebhookSubscriptionIsReconciledPeriodically(t *testing.T) {
	subscriber := &recordingSubscriber{calls: make(chan subscriptionCall, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		maintainWebhookSubscription(ctx, subscriber, "https://bot.example/webhook", "secret", []string{"message_created"}, 5*time.Millisecond)
		close(done)
	}()

	select {
	case call := <-subscriber.calls:
		if call.url != "https://bot.example/webhook" || call.secret != "secret" || len(call.updateTypes) != 1 || call.updateTypes[0] != "message_created" {
			t.Fatalf("unexpected reconciliation call: %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook subscription was not reconciled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("webhook reconciler did not stop")
	}
}
