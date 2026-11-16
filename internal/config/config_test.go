package config

import (
	"slices"
	"strings"
	"testing"
)

func TestRotationKeys(t *testing.T) {
	t.Setenv("LLMR_DATABASE_URL", "postgres://x")
	t.Setenv("LLMR_ENCRYPTION_KEY", "current-key-0123456789")
	t.Setenv("LLMR_ENCRYPTION_KEYS_OLD", " old-key-a-0123456789, old-key-b-0123456789 ,")
	t.Setenv("LLMR_ENCRYPTION_KEY_NEXT", "next-key-0123456789")
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"old-key-a-0123456789", "old-key-b-0123456789", "next-key-0123456789"}
	if !slices.Equal(c.OtherKeys(), want) {
		t.Errorf("OtherKeys = %q", c.OtherKeys())
	}
	for _, bad := range []struct{ env, val, msg string }{
		{"LLMR_ENCRYPTION_KEYS_OLD", "short", "at least"},
		{"LLMR_ENCRYPTION_KEYS_OLD", "change-me", "placeholder"},
		{"LLMR_ENCRYPTION_KEY_NEXT", "current-key-0123456789", "repeats"},
		{"LLMR_ENCRYPTION_KEYS_OLD", "next-key-0123456789", "repeats"},
	} {
		t.Run(bad.env+"="+bad.val, func(t *testing.T) {
			t.Setenv(bad.env, bad.val)
			if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), bad.msg) {
				t.Errorf("err = %v, want %q", err, bad.msg)
			}
		})
	}
}
