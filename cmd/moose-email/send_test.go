package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The send engine is built wherever the OAuth server is, and live sending stays
// off unless the config turns it on.
func TestSendEngineIsWiredAndLiveSendingStaysOff(t *testing.T) {
	cfg := oauthConfig(t)
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil || w == nil || w.send == nil {
		t.Fatalf("buildConnect = %v, %v", w, err)
	}
	defer w.store.Close()
	if w.send.Live {
		t.Fatal("email.send.live must default to false")
	}
	if w.send.RPID != cfg.Email.RPID || string(w.send.ReceiptKey) != cfg.Email.ReceiptPublicKey {
		t.Errorf("the engine must verify receipts against the pinned key and rp id: %+v", w.send)
	}

	cfg.Email.Send.Live = true
	w2, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.store.Close()
	if !w2.send.Live {
		t.Error("email.send.live: true was not passed to the engine")
	}

	// Without the OAuth server there is no approval service, so no engine.
	plain := devConfig(t)
	w3, err := buildConnect(context.Background(), plain, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w3.store.Close()
	if w3.send != nil {
		t.Error("a deployment without an approval service must not have a send engine")
	}
}

func TestKillSwitchCommand(t *testing.T) {
	cfg := devConfig(t)
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := killSwitch(ctx, w.store, args, &out, time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := run("status"); !strings.Contains(got, "off") {
		t.Errorf("status = %q", got)
	}
	if got := run("on", "suspected", "phishing"); !strings.Contains(got, "ON") {
		t.Errorf("on = %q", got)
	}
	ks, _ := w.store.KillSwitch(ctx)
	if !ks.On || ks.Reason != "suspected phishing" {
		t.Fatalf("stored = %+v", ks)
	}
	if got := run("status"); !strings.Contains(got, "ON") || !strings.Contains(got, "suspected phishing") {
		t.Errorf("status = %q", got)
	}
	run("off")
	if ks, _ = w.store.KillSwitch(ctx); ks.On {
		t.Fatal("off did not turn it off")
	}
	var out bytes.Buffer
	if err := killSwitch(ctx, w.store, []string{"maybe"}, &out, time.Now()); err == nil {
		t.Error("an unknown subcommand must be refused")
	}
	if err := runKillSwitch(ctx, nil, &out, noAWS); err == nil {
		t.Error("no subcommand must print the usage")
	}
}

func TestKillSwitchCanBeScopedToOneProvider(t *testing.T) {
	cfg := devConfig(t)
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	ctx := context.Background()
	at := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := killSwitch(ctx, w.store, args, &out, at); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := run("on", "provider=microsoft", "sent", "mismatch"); !strings.Contains(got, "microsoft") || !strings.Contains(got, "ON") {
		t.Errorf("on = %q", got)
	}
	if ks, _ := w.store.ProviderKillSwitch(ctx, "microsoft"); !ks.On || ks.Reason != "sent mismatch" {
		t.Fatalf("microsoft switch = %+v", ks)
	}
	if ks, _ := w.store.KillSwitch(ctx); ks.On {
		t.Fatal("a provider switch must not set the global one")
	}
	if got := run("status", "provider=microsoft"); !strings.Contains(got, "ON") || !strings.Contains(got, "sent mismatch") {
		t.Errorf("status = %q", got)
	}
	if got := run("status"); !strings.Contains(got, "off") {
		t.Errorf("global status = %q", got)
	}
	run("off", "provider=microsoft")
	if ks, _ := w.store.ProviderKillSwitch(ctx, "microsoft"); ks.On {
		t.Fatal("off did not turn the provider switch off")
	}
	var out bytes.Buffer
	if err := killSwitch(ctx, w.store, []string{"on", "provider=yahoo"}, &out, at); err == nil {
		t.Error("an unknown provider must be refused")
	}
}
