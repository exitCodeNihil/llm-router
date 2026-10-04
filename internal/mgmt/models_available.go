package mgmt

import (
	"net/http"
	"sort"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/gateway"
)

// availableModel is what a member needs to know to call a model: its name,
// the list price, and whether it is answering right now. Nothing about which
// provider row backs it — that is the admin's routing table.
type availableModel struct {
	Name        string   `json:"name"`
	InputPer1M  *float64 `json:"input_per_1m"`
	OutputPer1M *float64 `json:"output_per_1m"`
	Backends    int      `json:"backends"`
	Via         []string `json:"via"` // provider types, e.g. openrouter, gcp_vertex
	Flavor      string   `json:"api_flavor"`
	// Passthrough models forward the caller's own upstream token (Claude on
	// a subscription): they answer only clients that send one.
	Passthrough bool   `json:"passthrough"`
	Status      string `json:"status"` // ok | degraded | down
}

// availableModels lists every model the gateway serves, for any signed-in
// user. A key's allowed_models may narrow this further per client.
func (m *Server) availableModels(w http.ResponseWriter, r *http.Request) {
	out := []availableModel{}
	snap := m.Snapshots.Get()
	if snap == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	cooling := map[string]bool{}
	for _, c := range gateway.CoolingDeployments() {
		cooling[c.DeploymentID] = true
	}
	// The caller's own policy: what a personal key of theirs may call.
	me := &auth.Identity{User: snap.UsersByID[CallerFrom(r.Context()).UserID]}
	for _, name := range snap.ModelNames() {
		if !me.ModelAllowed(name) {
			continue
		}
		ds := snap.DeploymentsByModel[name]
		am := availableModel{Name: name, Backends: len(ds), Passthrough: snap.CallerCredentialOnly(name), Status: "ok"}
		seen := map[string]bool{}
		cold := 0
		for i, d := range ds {
			if cooling[d.ID] {
				cold++
			}
			if d.Provider != nil && !seen[d.Provider.Type] {
				seen[d.Provider.Type] = true
				am.Via = append(am.Via, d.Provider.Type)
			}
			if i == 0 {
				am.Flavor = d.APIFlavor
			}
			// Price of the first backend that has one: routing prefers it.
			// Pass-through traffic is billed to the caller's own subscription,
			// so it carries no price here.
			if am.InputPer1M == nil && !am.Passthrough {
				switch {
				case d.InputPer1M != nil && d.OutputPer1M != nil:
					am.InputPer1M, am.OutputPer1M = d.InputPer1M, d.OutputPer1M
				case d.CatalogModelID != "":
					if p, ok := snap.Prices[d.CatalogModelID]; ok {
						in, out := p.InputPer1M, p.OutputPer1M
						am.InputPer1M, am.OutputPer1M = &in, &out
					}
				}
			}
		}
		switch {
		case len(ds) > 0 && cold == len(ds):
			am.Status = "down"
		case cold > 0:
			am.Status = "degraded"
		}
		out = append(out, am)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}
