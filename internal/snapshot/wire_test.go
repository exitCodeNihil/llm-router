package snapshot

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func TestWireRoundTrip(t *testing.T) {
	in := 0.5
	h := sha256.Sum256([]byte("llmr_secret"))
	prov := &Provider{ID: "p1", Name: "vllm", Type: "openai_compatible", BaseURL: "http://x", AuthMode: "none", APIKey: "sk"}
	s := &Snapshot{
		Version:      42,
		KeysByHash:   map[[32]byte]*Key{h: {ID: "k1", UserID: "u1", BudgetUSD: &in}},
		UsersByID:    map[string]*User{"u1": {ID: "u1", Email: "Dev@X.com"}},
		UsersByEmail: map[string]*User{"dev@x.com": {ID: "u1", Email: "Dev@X.com"}},
		TeamsByID:    map[string]*Team{"t1": {ID: "t1", Name: "team"}},
		DeploymentsByModel: map[string][]*Deployment{
			"m": {{ID: "d1", Provider: prov, ModelName: "m", UpstreamName: "up", InputPer1M: &in}},
		},
		Prices:  map[string]Price{"azure/x": {InputPer1M: 1, OutputPer1M: 2}},
		Issuers: []*Issuer{{ID: "i1", IssuerURL: "https://iss", Audience: "aud", EmailClaim: "email"}},
		Spend:   map[string]Spend{"key:k1": {USD: 0.25}},
	}

	data, err := json.Marshal(ToWire(s))
	if err != nil {
		t.Fatal(err)
	}
	var w Wire
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	got := w.Snapshot()

	if got.Version != 42 {
		t.Errorf("version = %d", got.Version)
	}
	k := got.KeysByHash[h]
	if k == nil || k.ID != "k1" || *k.BudgetUSD != 0.5 {
		t.Fatalf("key lost in round trip: %+v", k)
	}
	if got.UsersByEmail["dev@x.com"] == nil {
		t.Error("email index not rebuilt")
	}
	ds := got.DeploymentsByModel["m"]
	if len(ds) != 1 || ds[0].Provider == nil || ds[0].Provider.APIKey != "sk" {
		t.Fatalf("deployment/provider lost: %+v", ds)
	}
	if got.Spend["key:k1"].USD != 0.25 || got.Prices["azure/x"].OutputPer1M != 2 {
		t.Error("spend/prices lost")
	}
	if len(got.Issuers) != 1 || got.Issuers[0].Audience != "aud" {
		t.Error("issuers lost")
	}
}
