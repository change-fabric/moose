package api

import (
	"strings"
	"testing"
)

func TestCanonicalSortsKeysAndKeepsHTMLUnescaped(t *testing.T) {
	got, err := Canonical([]byte(` { "b": 1, "a": {"z": "<tag>&", "y": [3, 2, 1.50]}, "c": null } `))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"y":[3,2,1.50],"z":"<tag>&"},"b":1,"c":null}`
	if string(got) != want {
		t.Fatalf("Canonical = %s, want %s", got, want)
	}
	again, _ := Canonical(got)
	if string(again) != want {
		t.Fatal("Canonical is not idempotent")
	}
	if SHA256(got) != SHA256(again) || len(SHA256(got)) != 64 {
		t.Fatal("bad sha256")
	}
}

func TestCanonicalRejectsGarbage(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a":1} {"b":2}`, `nope`} {
		if _, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%q) succeeded", in)
		}
	}
}

func TestParsePayloadPerKind(t *testing.T) {
	sha := strings.Repeat("a", 64)
	good := map[string]string{
		KindAccountSendEnable: `{"owner":"o","account_id":"a","from_address":"f@x.y"}`,
		KindEmailSend:         `{"owner":"o","account_id":"a","from_address":"f@x.y","draft_id":"d","rfc822_sha256":"` + sha + `","recipients":["r@x.y"],"soft_flags":[],"trust_changes":[]}`,
		KindContactTrustBatch: `{"owner":"o","account_id":"a","changes":[{"address":"r@x.y","to":"trusted"}]}`,
		KindDashboardSession:  `{"owner":"o","client_id":"c","client_name":"n","redirect_uri_host":"h","txn_hash":"t"}`,
		KindEmailSendBatch:    `{"owner":"o","account_id":"a","from_address":"f@x.y","items":[{"request_id":"r","payload_sha256":"` + sha + `"}]}`,
	}
	for kind, raw := range good {
		p, err := ParsePayload(kind, []byte(raw))
		if err != nil || p.Owner != "o" || p.SHA256 == "" {
			t.Errorf("%s: %+v, %v", kind, p, err)
		}
	}
	bad := map[string]string{
		KindAccountSendEnable: `{"owner":"o","account_id":"a"}`,
		KindEmailSend:         `{"owner":"o","account_id":"a","from_address":"f@x.y","draft_id":"d","rfc822_sha256":"` + sha + `","recipients":["r@x.y"]}`,
		KindContactTrustBatch: `{"owner":"o","account_id":"a","changes":[]}`,
		KindDashboardSession:  `{"owner":"o","client_id":"c","client_name":"n","redirect_uri_host":"h"}`,
		KindEmailSendBatch:    `{"owner":"o","account_id":"a","from_address":"f@x.y","items":[]}`,
	}
	for kind, raw := range bad {
		if _, err := ParsePayload(kind, []byte(raw)); err == nil {
			t.Errorf("%s: %s was accepted", kind, raw)
		}
	}
}

func TestFromKinds(t *testing.T) {
	for kind, want := range map[string]bool{
		KindEmailSend: true, KindEmailSendBatch: true, KindAccountSendEnable: true,
		KindContactTrustBatch: false, KindDashboardSession: false,
	} {
		if FromKinds(kind) != want {
			t.Errorf("FromKinds(%s) = %v", kind, !want)
		}
	}
}
