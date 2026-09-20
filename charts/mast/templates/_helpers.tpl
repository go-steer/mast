{{/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/*
Object names are FIXED, not prefixed with the release name.

Helm convention is `{{ .Release.Name }}-mast`, and the convention is
wrong here. Three of these names are load-bearing outside the chart:
scripts/setup-wif.sh binds an IAM principal derived from the daemon's
ServiceAccount name, scripts/rbac-matrix.sh checks grants by subject
name, and the WIF username the API server sees embeds both the
namespace and the ServiceAccount. A release-name prefix would make
every one of those depend on what the operator typed after
`helm install`.

Two releases in one cluster is not a supported topology regardless —
the ClusterRole and ClusterRoleBinding names are cluster-scoped and
would collide whatever we prefix them with.
*/}}
{{- define "mast.daemonSA" -}}mast-daemon{{- end -}}
{{- define "mast.watcherSA" -}}k8s-event-watcher{{- end -}}
{{- define "mast.daemonName" -}}mast{{- end -}}
{{- define "mast.watcherName" -}}k8s-event-watcher{{- end -}}
{{- define "mast.workloadConfigMap" -}}mast-workload{{- end -}}
{{- define "mast.gcpEnvConfigMap" -}}mast-gcp-env{{- end -}}
{{- define "mast.readRole" -}}mast-daemon-read{{- end -}}
{{- define "mast.writeRole" -}}mast-daemon-write{{- end -}}

{{/*
mast.projectID is the one required value, resolved in exactly one place
so the error message is the same wherever the omission is first noticed.
*/}}
{{- define "mast.projectID" -}}
{{- required "gcp.projectID is required: it is substituted into the RBAC subject for the GKE MCP path, and a placeholder there renders a ClusterRoleBinding that binds nobody (#290). Pass --set gcp.projectID=<your-project>." .Values.gcp.projectID -}}
{{- end -}}

{{/*
mast.wifUser is the RBAC username the API server gives the daemon's
Workload Identity Federation principal.

This is a User, not a ServiceAccount, and that is the whole point. mast
never presents the pod's KSA token to the API server: its tools call
container.googleapis.com/mcp with a Google credential derived from the
KSA, and the API server sees the federation principal. Measured against
a live GKE cluster on 2026-09-06 (#290): with only the ServiceAccount
subject bound, every MCP-path call is Forbidden — including reads, and
including in a namespace the write Role was applied to.

Every binding that names the daemon names both subjects. See
templates/rbac-daemon-write.yaml and
templates/clusterrolebinding-daemon-read.yaml, and
TestBindingsNameTheMCPPathSubject, which fails a binding naming only
one.
*/}}
{{- define "mast.wifUser" -}}
serviceAccount:{{ include "mast.projectID" . }}.svc.id.goog[{{ .Release.Namespace }}/{{ include "mast.daemonSA" . }}]
{{- end -}}

{{/*
mast.daemonSubjects is the subject list every daemon binding uses. It
exists so that "both subjects" is one edit rather than a rule someone
has to remember at each binding.
*/}}
{{- define "mast.daemonSubjects" -}}
- kind: ServiceAccount
  name: {{ include "mast.daemonSA" . }}
  namespace: {{ .Release.Namespace }}
- kind: User
  apiGroup: rbac.authorization.k8s.io
  name: {{ include "mast.wifUser" . }}
{{- end -}}

{{/*
mast.labels / mast.commonLabels — standard recommended labels plus
whatever the operator added.
*/}}
{{- define "mast.labels" -}}
app.kubernetes.io/part-of: mast
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
mast.image resolves the daemon image reference. An empty tag means the
chart's appVersion — the mast release this chart was published with —
rather than :latest.
*/}}
{{- define "mast.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{/*
mast.workloadKey flattens a bundle-relative path into a ConfigMap key.
ConfigMap keys cannot contain "/", so the workload tree is carried flat
and rebuilt in the pod by the volume's items: list.

Both ends of that round trip derive from the same glob over files/, so
the enumeration cannot drift from what is on disk — which under the
kustomize base was two hand-maintained lists and a test whose whole job
was catching the day they disagreed.
*/}}
{{- define "mast.workloadKey" -}}
{{ . | trimPrefix "files/" | replace "/" "_" }}
{{- end -}}
