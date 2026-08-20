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

package attach

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-steer/mast/pkg/auth"
)

// GET /sessions/{app}/{sid}/acl and PUT of the same — read and amend
// the access-control list of a live session.
//
// auth.ActionSessionAdmin has documented itself as covering "ACL /
// metadata mutations on the session" since the action matrix was
// written, and until now the only route behind it was DELETE: the ACL
// could be granted at creation and never afterwards touched. Adding a
// viewer to a running session meant deleting the session.
//
// GET is ActionSessionRead, not Admin. Anyone the ACL already admits
// can read the session's entire transcript; the list of who else is on
// it is strictly less sensitive than that, and a viewer who wants
// write access needs to know whom to ask. PUT is ActionSessionAdmin —
// owner or daemon admin.

// sessionACLResponse is the JSON body of GET /acl and of a successful
// PUT. Viewers and Contributors are always present as arrays (never
// null) so a client can append without a nil check.
type sessionACLResponse struct {
	Owner        string   `json:"owner"`
	Viewers      []string `json:"viewers"`
	Contributors []string `json:"contributors"`

	// Enforced reports whether this daemon actually consults the ACL.
	// False means MultiSessionEnabled is off and every authenticated
	// caller passes the session gate regardless of what is below —
	// worth saying out loud, because an ACL that reads plausibly and
	// governs nothing is the most misleading thing this endpoint
	// could return.
	Enforced bool `json:"enforced"`

	// Persisted reports whether this ACL survives a daemon restart.
	// False for a session registered without an owner (the legacy
	// Register path, which includes the daemon's own bootstrap
	// session) or on a daemon with no ACL store wired.
	Persisted bool `json:"persisted"`
}

// sessionACLRequest is the PUT body. It is a whole-document replace,
// not a patch: the viewers and contributors you send are the viewers
// and contributors the session ends up with. Omitting a key clears
// that list — a patch semantics that made omission mean "leave alone"
// would give an operator no way to remove the last viewer.
//
// Owner is optional and, when present, must either match the current
// owner (so a read-modify-write round trip works unchanged) or be a
// transfer, which only a daemon admin may perform.
type sessionACLRequest struct {
	Owner        *string  `json:"owner"`
	Viewers      []string `json:"viewers"`
	Contributors []string `json:"contributors"`
}

func (h *handlers) doGetSessionACL(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	writeJSON(w, http.StatusOK, h.aclResponse(entry.ACL(), h.wouldPersist(entry)))
}

// doSetSessionACL replaces the session's viewers and contributors, and
// optionally transfers ownership.
//
// Refuses with 501 when the daemon does not enforce ACLs. An
// amendment that is accepted, stored, and then consulted by nothing is
// the same defect shape as a declared-but-unenforced tool grant: the
// operator reads a success as "access is now restricted to these people"
// when in fact it is restricted to nobody. GET still answers on such a
// daemon — reporting enforced:false is exactly how an operator finds
// out why.
func (h *handlers) doSetSessionACL(w http.ResponseWriter, r *http.Request, entry *Entry) {
	if !h.enforceACL {
		http.Error(w, "PUT /acl not supported: this daemon does not enforce session ACLs (multi-session is off), so an amended ACL would govern nothing", http.StatusNotImplemented)
		return
	}

	var req sessionACLRequest
	if err := decodePOST(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode acl: %v", err), http.StatusBadRequest)
		return
	}

	current := entry.ACL()
	next := auth.SessionACL{Owner: current.Owner}

	if req.Owner != nil && *req.Owner != current.Owner {
		caller, _ := auth.CallerFromContext(r.Context())
		if !caller.Admin {
			// The owner reached this handler through
			// ActionSessionAdmin, so silently dropping the field
			// would leave them reading a 200 as a completed
			// transfer. Say no in a way they can see.
			http.Error(w, "transferring ownership requires a daemon admin caller; omit \"owner\" or send the current owner to amend viewers and contributors", http.StatusForbidden)
			return
		}
		if *req.Owner == "" {
			http.Error(w, "\"owner\" cannot be cleared: an owner-less session is reachable by admins only, which is a lockout rather than an edit", http.StatusBadRequest)
			return
		}
		next.Owner = *req.Owner
	}

	var err error
	if next.Viewers, err = normalizeIdentities("viewers", req.Viewers); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if next.Contributors, err = normalizeIdentities("contributors", req.Contributors); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	persisted, err := h.reg.SetACL(r.Context(), entry.AppName, entry.UserID, entry.SessionID, next)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		// Deleted between the lookup that authorized this request and
		// the write. Same 404 the lookup would have produced.
		http.Error(w, "session not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, fmt.Sprintf("amend acl: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, h.aclResponse(next, persisted))
}

func (h *handlers) aclResponse(acl auth.SessionACL, persisted bool) sessionACLResponse {
	out := sessionACLResponse{
		Owner:        acl.Owner,
		Viewers:      acl.Viewers,
		Contributors: acl.Contributors,
		Enforced:     h.enforceACL,
		Persisted:    persisted,
	}
	if out.Viewers == nil {
		out.Viewers = []string{}
	}
	if out.Contributors == nil {
		out.Contributors = []string{}
	}
	return out
}

// wouldPersist mirrors the predicate SetACL writes under, so GET and
// PUT can never disagree about whether this session's ACL is durable.
func (h *handlers) wouldPersist(entry *Entry) bool {
	_, hasStore := h.reg.aclStoreForList()
	return hasStore && entry.ACL().Owner != ""
}

// normalizeIdentities trims and de-duplicates an identity list,
// rejecting entries that are empty once trimmed. Order is preserved:
// the list an operator gets back from GET should look like the one
// they sent, so a diff against their own config is readable.
//
// Rejecting rather than dropping a blank entry is deliberate — a
// trailing "" in a viewers array is a client bug (a split on a stray
// comma, usually), and Authorize denies the empty identity anyway, so
// accepting it would store a grant that can never match.
func normalizeIdentities(field string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("%s[%d] is empty: an empty identity matches no caller", field, i)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
