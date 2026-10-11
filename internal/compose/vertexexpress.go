// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compose

import (
	"context"
	"net/http"
	"os"
	"strings"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/dialect/gemini"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Vertex AI express mode: Gemini on Vertex with an API key and no
// project.
//
// Before core-models, a gemini-* model with no --provider alias left
// backend selection to genai, and genai reads GOOGLE_GENAI_USE_VERTEXAI
// plus an API key with no project or location as express mode. The
// core-models Gemini adapter has no such mode — an api_key credential
// always means the Developer API, and its Vertex client requires a
// project and a location — so this file keeps the old behaviour by
// sending the request express mode sends: the Vertex backend, the
// publishers/ path with no projects/…/locations/… prefix, and the key
// in x-goog-api-key with no bearer token. It goes when the adapter can
// take an API key on Vertex itself.

// vertexExpressProject is the placeholder the adapter's Vertex client
// requires; vertexExpressTransport strips it from every path before the
// request leaves.
const vertexExpressProject = "mast-vertex-express"

// vertexExpressKey reports the API key express mode would run with, and
// whether it applies: no alias naming the backend outright (the
// `vertex` alias is ADC by design), GOOGLE_GENAI_USE_VERTEXAI set, a
// key, and no project or location in the environment. genai lets an
// implicit project or location win over an implicit key, and so does
// this.
func vertexExpressKey(provider string) (string, bool) {
	if provider == ProviderVertex || !geminiOnVertex(provider) {
		return "", false
	}
	for _, v := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"} {
		if os.Getenv(v) != "" {
			return "", false
		}
	}
	// genai's own order: GOOGLE_API_KEY wins when both are set.
	for _, v := range []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"} {
		if k := os.Getenv(v); k != "" {
			return k, true
		}
	}
	return "", false
}

// buildGeminiVertexExpress is buildGemini for express mode. opts carries
// the built-ins and the notice hook buildGemini already decided.
func buildGeminiVertexExpress(ctx context.Context, name, key string, opts coremodels.Options) (model.LLM, error) {
	bt, err := gemini.BuiltinToolsFromNames(opts.BuiltinTools)
	if err != nil {
		return nil, err
	}
	none, err := auth.Config{Kind: auth.None}.Resolve(ctx, auth.Options{})
	if err != nil {
		return nil, err
	}
	c, err := gemini.New(ctx, gemini.Options{
		Backend:      genai.BackendVertexAI,
		Project:      vertexExpressProject,
		Location:     "global",
		Credential:   none,
		BaseURL:      vendorBaseURL,
		HTTPClient:   &http.Client{Transport: vertexExpressTransport{key: key, base: http.DefaultTransport}},
		BackendName:  ProviderVertex,
		BuiltinTools: bt,
		Logf:         opts.Logf,
	})
	if err != nil {
		return nil, err
	}
	return bridged(c.Model(name)), nil
}

// vertexExpressTransport turns the adapter's project-scoped Vertex
// request into an express-mode one.
type vertexExpressTransport struct {
	key  string
	base http.RoundTripper
}

func (t vertexExpressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	prefix := "/projects/" + vertexExpressProject + "/locations/global"
	if i := strings.Index(r.URL.Path, prefix); i >= 0 {
		r.URL.Path = r.URL.Path[:i] + r.URL.Path[i+len(prefix):]
		r.URL.RawPath = ""
	}
	r.Header.Del("Authorization")
	r.Header.Set("x-goog-api-key", t.key)
	return t.base.RoundTrip(r)
}
