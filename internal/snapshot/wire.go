package snapshot

import "strings"

// Wire is the JSON form of a Snapshot for the edge sync protocol. Provider
// secrets ride along decrypted — the channel is TLS and node-token
// authenticated, and edge nodes must call upstreams themselves.
type Wire struct {
	Version     int64             `json:"version"`
	Keys        []WireKey         `json:"keys"`
	Users       []*User           `json:"users"`
	Teams       []*Team           `json:"teams"`
	Providers   []*Provider       `json:"providers"`
	Deployments []WireDeployment  `json:"deployments"`
	Prices      map[string]Price  `json:"prices"`
	Issuers     []*Issuer         `json:"issuers"`
	Spend       map[string]Spend  `json:"spend"`
	// Telemetry rides along so edges know whether to capture content; the
	// channel is TLS + node-token authenticated like provider secrets.
	Telemetry Telemetry `json:"telemetry"`
}

type WireKey struct {
	Hash []byte `json:"hash"`
	Key
}

type WireDeployment struct {
	ProviderID string `json:"provider_id"`
	Deployment
}

func ToWire(s *Snapshot) *Wire {
	w := &Wire{
		Version:   s.Version,
		Prices:    s.Prices,
		Issuers:   s.Issuers,
		Spend:     s.Spend,
		Telemetry: s.Telemetry,
	}
	for h, k := range s.KeysByHash {
		w.Keys = append(w.Keys, WireKey{Hash: append([]byte(nil), h[:]...), Key: *k})
	}
	for _, u := range s.UsersByID {
		w.Users = append(w.Users, u)
	}
	for _, t := range s.TeamsByID {
		w.Teams = append(w.Teams, t)
	}
	seen := map[string]bool{}
	for _, ds := range s.DeploymentsByModel {
		for _, d := range ds {
			if !seen[d.Provider.ID] {
				seen[d.Provider.ID] = true
				w.Providers = append(w.Providers, d.Provider)
			}
			wd := WireDeployment{ProviderID: d.Provider.ID, Deployment: *d}
			wd.Deployment.Provider = nil
			w.Deployments = append(w.Deployments, wd)
		}
	}
	return w
}

func (w *Wire) Snapshot() *Snapshot {
	s := &Snapshot{
		Version:            w.Version,
		Telemetry:          w.Telemetry,
		KeysByHash:         map[[32]byte]*Key{},
		UsersByID:          map[string]*User{},
		UsersByEmail:       map[string]*User{},
		TeamsByID:          map[string]*Team{},
		DeploymentsByModel: map[string][]*Deployment{},
		Prices:             w.Prices,
		Issuers:            w.Issuers,
		Spend:              w.Spend,
	}
	if s.Prices == nil {
		s.Prices = map[string]Price{}
	}
	if s.Spend == nil {
		s.Spend = map[string]Spend{}
	}
	for i := range w.Keys {
		if len(w.Keys[i].Hash) != 32 {
			continue
		}
		var h [32]byte
		copy(h[:], w.Keys[i].Hash)
		s.KeysByHash[h] = &w.Keys[i].Key
	}
	for _, u := range w.Users {
		s.UsersByID[u.ID] = u
		s.UsersByEmail[strings.ToLower(u.Email)] = u
	}
	for _, t := range w.Teams {
		s.TeamsByID[t.ID] = t
	}
	providers := map[string]*Provider{}
	for _, p := range w.Providers {
		providers[p.ID] = p
	}
	for i := range w.Deployments {
		d := w.Deployments[i].Deployment
		d.Provider = providers[w.Deployments[i].ProviderID]
		if d.Provider == nil {
			continue
		}
		dd := d
		s.DeploymentsByModel[d.ModelName] = append(s.DeploymentsByModel[d.ModelName], &dd)
	}
	// Re-sort rather than trust the wire order: the rule lives in one place.
	s.SortDeployments()
	return s
}
