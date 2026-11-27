package main

import (
	"context"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/config"
)

// approvalConfig is providerConfig plus what the invite engine needs: the
// OAuth identity, the pinned receipt key and the shared service secret.
func approvalConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := providerConfig(t)
	cfg.Email.BaseURL = "http://localhost:8080"
	cfg.Email.Owner, cfg.Email.RPID = "pat", "localhost"
	cfg.Email.ReceiptPublicKey = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
	cfg.Email.ApproverSecret = strings.Repeat("s", 32)
	return cfg
}

func TestTheInviteEngineIsBuiltOnlyWithTheApprovalServiceConfigured(t *testing.T) {
	ctx := context.Background()
	w, err := buildWiring(ctx, approvalConfig(t), quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	if w.invites == nil || w.invites.Live || w.invites.RPID != "localhost" || w.invites.Providers == nil || w.invites.Seal == nil {
		t.Fatalf("invites = %+v: want an engine that is not live by default", w.invites)
	}

	live := approvalConfig(t)
	live.Email.Calendar.Invite.Live = true
	wl, err := buildWiring(ctx, live, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer wl.store.Close()
	if wl.invites == nil || !wl.invites.Live {
		t.Fatal("email.calendar.invite.live did not reach the engine")
	}

	// No approval service configured: no engine, and attendee writes stay invites_not_enabled.
	plain, err := buildWiring(ctx, providerConfig(t), quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.store.Close()
	if plain.invites != nil {
		t.Fatal("an invite engine was built with no approval service configured")
	}

	short := approvalConfig(t)
	short.Email.ApproverSecret = "too-short"
	if _, err := buildWiring(ctx, short, quiet, noAWS); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("a short approver secret: %v", err)
	}
}
