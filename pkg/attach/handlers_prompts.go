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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2

package attach

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-steer/mast/pkg/auth"
)

// PR D — HTTP-driven permission prompts. Two endpoints:
//
//	GET  /sessions/<sid>/perms/stream    SSE stream of pending prompts
//	POST /sessions/<sid>/perms/respond   operator's decision
//
// The remote TUI's adapter subscribes to /perms/stream; each frame
// becomes a coretui.PermissionRequest displayed in the host TUI's
// modal. When the operator picks a decision, the adapter POSTs to
// /perms/respond.
//
// What is on the other end is a PermsSource, not necessarily a
// PromptBroker (#364): in core-agent the answer unblocks a gate
// sitting inside AskApproval, and in mast it resolves a durable
// write-gate park. permsource.go carries the reason the seam is an
// interface. Agents offering neither capability get 501 for both —
// matching the "capability not registered" convention used by the
// other PR A2 mutators.

func (h *handlers) registerPrompts(mux *http.ServeMux) {
	h.routeSession(mux, "GET", "perms/stream", auth.ActionSessionRead, h.doPermsStream)
	h.routeSession(mux, "POST", "perms/respond", auth.ActionSessionWrite, h.doPermsRespond)
}

func (h *handlers) doPermsStream(w http.ResponseWriter, r *http.Request, entry *Entry) {
	source := permsSourceFor(entry)
	if source == nil {
		http.Error(w, "perms/stream capability not registered", http.StatusNotImplemented)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "server does not support streaming (no http.Flusher)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	frames, cleanup := source.SubscribePrompts(r.Context())
	defer cleanup()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closing:
			// Server shutdown: end the stream so srv.Shutdown can
			// drain instead of waiting out its full timeout on a
			// client that would otherwise stay attached (#488). The
			// client sees EOF and re-subscribes after reconnect.
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			buf, jerr := json.Marshal(frame)
			if jerr != nil {
				continue
			}
			if _, werr := fmt.Fprintf(w, "event: prompt\ndata: %s\n\n", buf); werr != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *handlers) doPermsRespond(w http.ResponseWriter, r *http.Request, entry *Entry) {
	source := permsSourceFor(entry)
	if source == nil {
		http.Error(w, "perms/respond capability not registered", http.StatusNotImplemented)
		return
	}

	var req PromptResponse
	if err := readJSON(r, &req, operatorPostMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "perms/respond: id is required", http.StatusBadRequest)
		return
	}
	decision, ok := DecisionFromWire(req.Decision)
	if !ok {
		http.Error(w, fmt.Sprintf("perms/respond: unknown decision %q (want deny|allow-once|allow-session|allow-session-verb|allow-session-tool|allow-always)", req.Decision), http.StatusBadRequest)
		return
	}
	approver, err := source.RespondPrompt(r.Context(), req.ID, decision)
	if err != nil {
		switch {
		case errors.Is(err, ErrPromptNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrDecisionNotAdmissible):
			// 400, not 403: the operator is allowed to answer, and the
			// answer they sent is one this question does not take. The
			// fix is in the client's button set.
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// approver is omitted when the source recorded nobody, which is the
	// honest answer for a PromptBroker: it releases a blocked call and
	// writes down no identity. A durable source reports the identity the
	// decision was stamped with, so a client can name who approved
	// instead of asserting that somebody did.
	out := map[string]any{"acknowledged": true}
	if approver != "" {
		out["approver"] = approver
	}
	writeJSON(w, http.StatusOK, out)
}
