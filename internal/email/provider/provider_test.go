package provider_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
)

func TestCapabilitiesJSONNames(t *testing.T) {
	b, err := json.Marshal(provider.Capabilities{Threads: true, Labels: true, SearchDialect: provider.DialectGmail})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"threads":true,"labels":true,"search_dialect":"gmail","native_draft_send":false}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}

func TestRateLimitErrorWrapsTheSentinel(t *testing.T) {
	var err error = &provider.RateLimitError{RetryAfter: 3 * time.Second}
	wrapped := fmt.Errorf("account a: %w", err)
	if !errors.Is(wrapped, provider.ErrRateLimited) {
		t.Fatal("a wrapped RateLimitError must satisfy errors.Is(ErrRateLimited)")
	}
	var rl *provider.RateLimitError
	if !errors.As(wrapped, &rl) || rl.RetryAfter != 3*time.Second {
		t.Fatalf("errors.As = %v, %+v", errors.As(wrapped, &rl), rl)
	}
	if !strings.Contains(err.Error(), "3s") {
		t.Errorf("message %q should mention the wait", err.Error())
	}
}

func TestNeutralMailboxIDs(t *testing.T) {
	got := []string{provider.MailboxInbox, provider.MailboxSent, provider.MailboxDraft, provider.MailboxTrash, provider.MailboxSpam}
	want := []string{"INBOX", "SENT", "DRAFT", "TRASH", "SPAM"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mailbox id %d = %q, want %q", i, got[i], want[i])
		}
	}
	if provider.DialectGraph != "graph" {
		t.Errorf("DialectGraph = %q", provider.DialectGraph)
	}
}
