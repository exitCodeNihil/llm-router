// Package redact removes personal data and secrets from captured prompts and
// responses before they are exported. It works on text: JSON is walked and
// only its strings are changed, so what comes out still parses.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// Config is the redaction part of the capture policy.
type Config struct {
	Enabled bool `json:"enabled"`
	// Builtin names the built-in detectors in use; nil means all of Kinds.
	Builtin []string  `json:"builtin"`
	Custom  []Pattern `json:"custom"`
}

// Pattern is an RE2 expression of the operator's own. Matches are replaced
// with [REDACTED:<name>].
type Pattern struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// Kinds are the built-in detectors.
var Kinds = []string{"email", "card", "secret", "private_key"}

const (
	maxCustom  = 50
	maxPattern = 1000
)

var builtin = map[string]*regexp.Regexp{
	"email": regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`),
	// 13 to 19 digits, optionally grouped; kept only when the Luhn sum holds.
	"card": regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`),
	"secret": regexp.MustCompile(strings.Join([]string{
		`(?i:\bbearer\s+)[A-Za-z0-9._~+/\-]{16,}=*`,
		`\b(?:sk|pk|rk)-[A-Za-z0-9_\-]{16,}`,
		`\bllmrg?_[A-Za-z0-9]{20,}`,
		`\bgh[pousr]_[A-Za-z0-9]{30,}`,
		`\bgithub_pat_[A-Za-z0-9_]{30,}`,
		`\bxox[abprs]-[A-Za-z0-9\-]{10,}`,
		`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
		`\bAIza[0-9A-Za-z_\-]{35}`,
		`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`,
	}, "|")),
	// To the END line, or to the end of the text when the capture cut it off.
	"private_key": regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`),
}

type rule struct {
	kind string // counted under this
	tag  string // [REDACTED:tag]
	re   *regexp.Regexp
	luhn bool
}

// Redactor is a compiled Config. The zero value and nil change nothing.
type Redactor struct{ rules []rule }

// Compile checks c and builds its Redactor: nil when redaction is off.
func Compile(c Config) (*Redactor, error) {
	if !c.Enabled {
		return nil, nil
	}
	r := &Redactor{}
	kinds := c.Builtin
	if kinds == nil {
		kinds = Kinds
	}
	// Keys first: a card or email rule must not break up a longer secret.
	for _, k := range []string{"private_key", "secret", "email", "card"} {
		for _, want := range kinds {
			if want == k {
				r.rules = append(r.rules, rule{kind: k, tag: k, re: builtin[k], luhn: k == "card"})
			}
		}
	}
	for _, k := range kinds {
		if builtin[k] == nil {
			return nil, fmt.Errorf("unknown detector %q", k)
		}
	}
	if len(c.Custom) > maxCustom {
		return nil, fmt.Errorf("at most %d custom patterns", maxCustom)
	}
	for i, p := range c.Custom {
		name := strings.TrimSpace(p.Name)
		if name == "" || len(name) > 40 || strings.ContainsAny(name, "[]\"\\\n") {
			return nil, fmt.Errorf("custom pattern %d: name must be 1 to 40 characters without brackets or quotes", i+1)
		}
		if p.Pattern == "" || len(p.Pattern) > maxPattern {
			return nil, fmt.Errorf("custom pattern %q: pattern must be 1 to %d characters", name, maxPattern)
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("custom pattern %q: %v", name, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("custom pattern %q matches empty text", name)
		}
		r.rules = append(r.rules, rule{kind: "custom", tag: name, re: re})
	}
	return r, nil
}

var (
	countMu sync.Mutex
	counts  = map[string]*atomic.Int64{}
)

func count(kind string) {
	countMu.Lock()
	c := counts[kind]
	if c == nil {
		c = &atomic.Int64{}
		counts[kind] = c
	}
	countMu.Unlock()
	c.Add(1)
}

// Counts is how many values were redacted since the process started, by kind.
func Counts() map[string]int64 {
	countMu.Lock()
	defer countMu.Unlock()
	out := make(map[string]int64, len(counts))
	for k, c := range counts {
		out[k] = c.Load()
	}
	return out
}

// String returns s with every match replaced.
func (r *Redactor) String(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, ru := range r.rules {
		if !ru.re.MatchString(s) {
			continue
		}
		s = ru.re.ReplaceAllStringFunc(s, func(m string) string {
			if ru.luhn && !luhn(m) {
				return m
			}
			count(ru.kind)
			return "[REDACTED:" + ru.tag + "]"
		})
	}
	return s
}

// JSON redacts the strings inside a JSON document: every string value, the
// JSON a string value itself holds (tool-call arguments), and numbers that
// are card numbers. Object keys are left alone. Text that does not parse,
// because the capture cut it off, is treated as plain text.
func (r *Redactor) JSON(raw json.RawMessage) json.RawMessage {
	if r == nil || len(raw) == 0 {
		return raw
	}
	out, ok := r.document(raw)
	if !ok {
		return json.RawMessage(r.String(string(raw)))
	}
	return out
}

func (r *Redactor) document(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.value("", v)); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

func (r *Redactor) value(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = r.value(k, e)
		}
	case []any:
		for i, e := range t {
			t[i] = r.value(key, e)
		}
	case string:
		// Encoded media: nothing to find, and megabytes to scan.
		if key == "data" || strings.HasPrefix(t, "data:") {
			return t
		}
		if s := strings.TrimLeft(t, " \t\r\n"); s != "" && (s[0] == '{' || s[0] == '[') {
			if out, ok := r.document([]byte(t)); ok {
				return string(out)
			}
		}
		return r.String(t)
	case json.Number:
		if s := t.String(); len(s) >= 13 && r.String(s) != s {
			return "[REDACTED:card]"
		}
	}
	return v
}

// luhn reports whether the digits in s pass the card-number check.
func luhn(s string) bool {
	sum, n, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		n++
	}
	return n >= 13 && n <= 19 && sum%10 == 0
}
