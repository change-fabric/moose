package provider_test

import (
	"encoding/json"
	"testing"

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
