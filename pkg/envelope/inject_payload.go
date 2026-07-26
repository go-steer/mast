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

// Package envelope defines the on-the-wire payload shapes mast accepts on
// its inject endpoint. Field names and JSON casing mirror the emitters
// that produce them so consumers can pattern-match against a single
// stable schema.
package envelope

import "time"

// InjectPayload is the JSON body POSTed to /inject by k8s-event-watcher
// (and any other edge-trigger source that speaks the same shape). The
// schema mirrors core-agent/cmd/k8s-event-watcher/types.go verbatim so
// the existing sidecar image can be repointed at mast without
// recompilation.
type InjectPayload struct {
	Kind         string         `json:"kind"`
	Reason       string         `json:"reason"`
	Namespace    string         `json:"namespace"`
	KindOfObject string         `json:"kind_of_object"`
	Name         string         `json:"name"`
	Container    string         `json:"container,omitempty"`
	UID          string         `json:"uid"`
	Message      string         `json:"message"`
	Count        int            `json:"count"`
	FirstSeen    time.Time      `json:"first_seen"`
	LastSeen     time.Time      `json:"last_seen"`
	Cluster      string         `json:"cluster"`
	Context      PayloadContext `json:"context"`
}

// PayloadContext is the nested "context" object on InjectPayload.
type PayloadContext struct {
	ControllerRef string            `json:"controller_ref,omitempty"`
	Node          string            `json:"node,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// Payload kind constants stamped by the emitter on InjectPayload.Kind.
// Consumers use these to distinguish signal sources.
const (
	KindK8sEvent         = "k8s-event"
	KindK8sEventFollowup = "k8s-event-followup"
)
