package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestNewRequestURLs(t *testing.T) {
	cases := []struct {
		name     string
		provider *snapshot.Provider
		flavor   string
		path     string
		wantURL  string
	}{
		{
			"azure openai deployments",
			&snapshot.Provider{Type: "azure", BaseURL: "https://r.services.ai.azure.com/", AuthMode: "none"},
			"openai", "/chat/completions",
			"https://r.services.ai.azure.com/openai/deployments/dep/chat/completions?api-version=2024-10-21",
		},
		{
			"foundry anthropic surface",
			&snapshot.Provider{Type: "azure", BaseURL: "https://r.services.ai.azure.com", AuthMode: "none"},
			"anthropic", "/chat/completions",
			"https://r.services.ai.azure.com/anthropic/v1/messages",
		},
		{
			"direct anthropic api",
			&snapshot.Provider{Type: "openai_compatible", BaseURL: "https://api.anthropic.com", AuthMode: "api_key", APIKey: "sk-ant-x"},
			"anthropic", "/messages",
			"https://api.anthropic.com/v1/messages",
		},
		{
			"direct anthropic api with /v1 base",
			&snapshot.Provider{Type: "openai_compatible", BaseURL: "https://api.anthropic.com/v1", AuthMode: "api_key", APIKey: "sk-ant-x"},
			"anthropic", "/messages",
			"https://api.anthropic.com/v1/messages",
		},
		{
			"openai compatible",
			&snapshot.Provider{Type: "openai_compatible", BaseURL: "http://vllm:8000/v1", AuthMode: "none"},
			"openai", "/chat/completions",
			"http://vllm:8000/v1/chat/completions",
		},
	}
	for _, tc := range cases {
		d := &snapshot.Deployment{Provider: tc.provider, UpstreamName: "dep", APIFlavor: tc.flavor}
		req, err := NewRequest(context.Background(), d, tc.path, []byte("{}"))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := req.URL.String(); got != tc.wantURL {
			t.Errorf("%s: url = %s, want %s", tc.name, got, tc.wantURL)
		}
		if tc.flavor == "anthropic" {
			if req.Header.Get("anthropic-version") == "" {
				t.Errorf("%s: missing anthropic-version header", tc.name)
			}
			if tc.provider.AuthMode == "api_key" && req.Header.Get("x-api-key") == "" {
				t.Errorf("%s: api_key auth must use x-api-key header", tc.name)
			}
		}
	}

	// embeddings must be rejected on anthropic-flavor deployments
	d := &snapshot.Deployment{Provider: &snapshot.Provider{Type: "azure", BaseURL: "https://x", AuthMode: "none"}, UpstreamName: "c", APIFlavor: "anthropic"}
	if _, err := NewRequest(context.Background(), d, "/embeddings", []byte("{}")); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("embeddings on anthropic flavor should error, got %v", err)
	}
}

func TestPerMillion(t *testing.T) {
	cases := map[string]*float64{"0.0000003": f(0.3), "0.00001": f(10), "0": f(0), "": nil, "-1": nil, "x": nil}
	for in, want := range cases {
		got := perMillion(in)
		switch {
		case want == nil && got != nil, want != nil && got == nil, want != nil && *got != *want:
			t.Errorf("perMillion(%q) = %v, want %v", in, got, want)
		}
	}
}

func f(v float64) *float64 { return &v }

func TestOpenRouterDefaultsBaseURL(t *testing.T) {
	d := &snapshot.Deployment{Provider: &snapshot.Provider{Type: "openrouter", AuthMode: "bearer", APIKey: "k"}, UpstreamName: "x/y:free"}
	req, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.String(); got != OpenRouterBase+"/chat/completions" {
		t.Errorf("url = %s", got)
	}
	if req.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("auth header missing")
	}
}
