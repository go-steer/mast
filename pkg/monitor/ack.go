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

// The ack vocabulary (v0.5 W4.6) — the two argument names mast fills in
// when it forwards an operator's acknowledgement.
//
// They live here, beside the transition records, because they are the
// same contract read backwards. A cycle learns a subject's key from the
// producer's classification and hands it to the model and the chat; an
// ack arrives naming one of those keys and goes back to the producer.
// mast never parses the key in either direction — see Transition.
//
// So these are not k8s spellings and are not lookout's to change
// unilaterally: they are the monitor contract's, and the k8s-shaped
// arguments (cluster, namespace, how long a suppression lasts) stay
// where domain facts have always gone, in the bundle's args map.
package monitor

const (
	// AckSubjectArg is the argument carrying WHAT was acked: the
	// producer's own subject key, verbatim from the classification.
	AckSubjectArg = "subject_key"

	// AckByArg is the argument carrying WHO acked. mast fills it from
	// the authenticated caller and from nowhere else — never from the
	// request body, and never from the bundle, both of which are
	// attributions a caller writes about itself.
	AckByArg = "ack_by"
)
