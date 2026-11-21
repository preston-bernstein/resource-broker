// Package inference holds the request type every layer shares when it runs one
// generation: the durable Job worker, the Backend interface and the routers.
package inference

// Request is one generation. OnTokens, if non-nil, receives the running token
// count as tokens are produced.
type Request struct {
	Model    string
	Prompt   string
	Options  map[string]any
	OnTokens func(int)
}
