package gateway

import (
	"encoding/json"
	"fmt"
)

// reqBody is a partial decode of an OpenAI-style request: we only interpret
// the fields the router needs and pass everything else through untouched.
type reqBody struct {
	fields       map[string]json.RawMessage
	Model        string
	Stream       bool
	IncludeUsage bool // client already asked for stream usage
}

func parseBody(raw []byte) (*reqBody, error) {
	b := &reqBody{fields: map[string]json.RawMessage{}}
	if err := json.Unmarshal(raw, &b.fields); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if m, ok := b.fields["model"]; ok {
		if err := json.Unmarshal(m, &b.Model); err != nil {
			return nil, fmt.Errorf("invalid model field: %w", err)
		}
	}
	if sRaw, ok := b.fields["stream"]; ok {
		_ = json.Unmarshal(sRaw, &b.Stream)
	}
	if so, ok := b.fields["stream_options"]; ok {
		var opts struct {
			IncludeUsage bool `json:"include_usage"`
		}
		_ = json.Unmarshal(so, &opts)
		b.IncludeUsage = opts.IncludeUsage
	}
	return b, nil
}

// upstreamBody re-encodes the request with the upstream model name and, when
// injectUsage is set, stream_options.include_usage=true (preserving any other
// stream_options the client sent).
func (b *reqBody) upstreamBody(upstreamName string, injectUsage bool) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(b.fields)+1)
	for k, v := range b.fields {
		out[k] = v
	}
	nameJSON, _ := json.Marshal(upstreamName)
	out["model"] = nameJSON
	if injectUsage {
		opts := map[string]json.RawMessage{}
		if so, ok := out["stream_options"]; ok {
			_ = json.Unmarshal(so, &opts)
		}
		opts["include_usage"] = json.RawMessage("true")
		soJSON, err := json.Marshal(opts)
		if err != nil {
			return nil, err
		}
		out["stream_options"] = soJSON
	}
	return json.Marshal(out)
}
