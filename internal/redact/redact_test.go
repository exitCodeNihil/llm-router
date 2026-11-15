package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, c Config) *Redactor {
	t.Helper()
	r, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestString(t *testing.T) {
	r := mustCompile(t, Config{Enabled: true, Custom: []Pattern{{Name: "ticket", Pattern: `TCK-\d{6}`}}})
	cases := []struct{ in, want string }{
		{"mail jane.doe+x@example.co.uk now", "mail [REDACTED:email] now"},
		{"card 4111 1111 1111 1111.", "card [REDACTED:card]."},
		{"card 4111-1111-1111-1112", "card 4111-1111-1111-1112"}, // fails the Luhn sum
		{"order 1234567890123", "order 1234567890123"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345", "Authorization: [REDACTED:secret]"},
		{"key sk-ant-api03-ABCDEFGHIJKLMNOPQRST", "key [REDACTED:secret]"},
		{"aws AKIAIOSFODNN7EXAMPLE", "aws [REDACTED:secret]"},
		{"see TCK-123456", "see [REDACTED:ticket]"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\nafter", "[REDACTED:private_key]\nafter"},
		{`-----BEGIN PRIVATE KEY-----\nMIIE cut off`, "[REDACTED:private_key]"},
		{"nothing here", "nothing here"},
	}
	for _, c := range cases {
		if got := r.String(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSONStaysValid(t *testing.T) {
	r := mustCompile(t, Config{Enabled: true})
	in := `[{"role":"user","content":[{"type":"text","text":"I am a@b.io"},` +
		`{"type":"image","source":{"data":"a@b.io","media_type":"image/png"}},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD@b.io"}}]},` +
		`{"role":"user","content":"data: my mail is e@f.io"},{"role":"tool","data":"g@h.io"},` +
		`{"role":"assistant","tool_calls":[{"function":{"name":"pay","arguments":"{\"card\":4111111111111111,\"to\":\"c@d.org\",\"n\":3}"}}]},` +
		`{"a@b.io":"key stays"}]`
	out := r.JSON(json.RawMessage(in))
	var v []map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	s := string(out)
	for _, gone := range []string{`"I am a@b.io"`, "c@d.org", "4111111111111111", "e@f.io", "g@h.io"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q survived: %s", gone, s)
		}
	}
	for _, kept := range []string{`"data":"a@b.io"`, `"a@b.io":"key stays"`, `\"n\":3`, "data:image/png;base64,QUJD@b.io"} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q changed: %s", kept, s)
		}
	}
	// The arguments string must still hold JSON.
	args := v[3]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !json.Valid([]byte(args)) {
		t.Errorf("arguments no longer JSON: %s", args)
	}
	// Cut-off JSON is still redacted, as text.
	if got := string(r.JSON(json.RawMessage(`[{"text":"x@y.zz`))); strings.Contains(got, "x@y.zz") {
		t.Errorf("cut-off input kept the address: %s", got)
	}
}

func TestCompile(t *testing.T) {
	if r, err := Compile(Config{}); r != nil || err != nil {
		t.Fatalf("off: %v %v", r, err)
	}
	var off *Redactor
	if off.String("a@b.io") != "a@b.io" || string(off.JSON([]byte(`"a@b.io"`))) != `"a@b.io"` {
		t.Fatal("nil redactor changed text")
	}
	only := mustCompile(t, Config{Enabled: true, Builtin: []string{"email"}})
	if got := only.String("a@b.io 4111111111111111"); got != "[REDACTED:email] 4111111111111111" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []Config{
		{Enabled: true, Builtin: []string{"nope"}},
		{Enabled: true, Custom: []Pattern{{Name: "x", Pattern: "("}}},
		{Enabled: true, Custom: []Pattern{{Name: "x", Pattern: "a*"}}},
		{Enabled: true, Custom: []Pattern{{Name: "", Pattern: "a"}}},
		{Enabled: true, Custom: []Pattern{{Name: "a]b", Pattern: "a"}}},
	} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
