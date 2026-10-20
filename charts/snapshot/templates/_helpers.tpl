# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

{{/*
Expand the name of the chart.
*/}}
{{- define "snapshot.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "snapshot.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "snapshot.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "snapshot.labels" -}}
helm.sh/chart: {{ include "snapshot.chart" . }}
{{ include "snapshot.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "snapshot.selectorLabels" -}}
app.kubernetes.io/name: {{ include "snapshot.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the agent service account to use
*/}}
{{- define "snapshot.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "snapshot.fullname" . ) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the operator service account.
*/}}
{{- define "snapshot.operatorServiceAccountName" -}}
{{- printf "%s-operator" (include "snapshot.fullname" .) }}
{{- end }}

{{/*
Use the same agent image for the DaemonSet and cuinterpose installer.
*/}}
{{- define "snapshot.agentImage" -}}
{{- printf "%s:%s" .Values.image.agent.repository (.Values.image.agent.tag | default .Chart.AppVersion) -}}
{{- end }}

{{/*
Convert a Kubernetes quantity to bytes, or empty when it is not a quantity.
Covers every form Kubernetes accepts for a memory size: a decimal number with
a binary suffix (Ki to Ei), a decimal suffix (n, u, m, k to E) or a decimal
exponent (e3, E-2). Fractional bytes round down.
*/}}
{{- define "snapshot.quantityBytes" -}}
{{- $q := . | toString | trim -}}
{{- $pattern := `^([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(Ki|Mi|Gi|Ti|Pi|Ei|[eE][+-]?[0-9]+|[numkMGTPE])?$` -}}
{{- if regexMatch $pattern $q -}}
{{- $number := regexReplaceAll $pattern $q "${1}" | float64 -}}
{{- $suffix := regexReplaceAll $pattern $q "${2}" -}}
{{- $multipliers := dict
      "" 1.0 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0
      "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0
      "n" 1e-9 "u" 1e-6 "m" 1e-3 "k" 1e3 "M" 1e6 "G" 1e9 "T" 1e12 "P" 1e15 "E" 1e18 -}}
{{- if hasKey $multipliers $suffix -}}
{{- mulf $number (get $multipliers $suffix) | floor | int64 -}}
{{- else -}}
{{- /* A decimal exponent: scale by ten once per unit of it. */ -}}
{{- $exponent := regexReplaceAll "^[eE]" $suffix "" | int -}}
{{- $steps := $exponent -}}
{{- if lt $exponent 0 }}{{ $steps = mul -1 $exponent }}{{ end -}}
{{- range until ($steps | int) -}}
{{- if gt $exponent 0 }}{{ $number = mulf $number 10 }}{{ else }}{{ $number = divf $number 10 }}{{ end -}}
{{- end -}}
{{- $number | floor | int64 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The staging sizeLimit, lowered to leave the agent room to survive filling it.
Sizing is in docs/operations/storage.md. Falls back when pageBroker.staging is
explicitly null, as helm upgrade --reuse-values leaves it.
*/}}
{{- define "snapshot.stagingSizeLimit" -}}
{{- $configured := ((.Values.pageBroker).staging | default dict).sizeLimit | default "56Gi" -}}
{{- /* Both containers write this volume — the agent on checkpoint, PageBroker
       on staged restore — and the pages charge to whichever wrote them, so the
       smaller limit is the one that has to hold. */ -}}
{{- /* Every valid quantity parses, so one that does not is invalid and
       Kubernetes would reject it anyway; fail here, where the message is clear,
       rather than leave the volume uncapped. */ -}}
{{- $limitBytes := "" -}}
{{- range $container := list (((.Values.daemonset).resources).limits).memory (((.Values.pageBroker).resources).limits).memory -}}
{{- if $container -}}
{{- $bytes := include "snapshot.quantityBytes" $container -}}
{{- if not $bytes -}}
{{- fail (printf "memory limit %q is not a Kubernetes quantity" (toString $container)) -}}
{{- end -}}
{{- if or (not $limitBytes) (lt (int64 $bytes) (int64 $limitBytes)) }}{{- $limitBytes = $bytes -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- $configuredBytes := include "snapshot.quantityBytes" $configured -}}
{{- if not $configuredBytes -}}
{{- fail (printf "pageBroker.staging.sizeLimit %q is not a Kubernetes quantity" (toString $configured)) -}}
{{- end -}}
{{- if not $limitBytes -}}
{{- $configured -}}
{{- else -}}
{{- /* Reserve 8Gi for the agent process and CRIU, or an eighth of a smaller limit. */ -}}
{{- $reserve := min (include "snapshot.quantityBytes" "8Gi" | int64) (div (int64 $limitBytes) 8) -}}
{{- $maxBytes := sub (int64 $limitBytes) $reserve -}}
{{- if le (int64 $configuredBytes) $maxBytes }}{{ $configured }}{{ else }}{{ $maxBytes }}{{ end -}}
{{- end -}}
{{- end }}

{{/*
Fail fast on unsupported runtime.type values. Called once from daemonset.yaml.
*/}}
{{- define "snapshot.validateRuntime" -}}
{{- if not (has .Values.runtime.type (list "containerd" "crio")) }}
{{- fail (printf "runtime.type must be 'containerd' or 'crio', got %q" .Values.runtime.type) }}
{{- end }}
{{- if and .Values.runtime.storageDir (not (hasPrefix "/" .Values.runtime.storageDir)) }}
{{- fail (printf "runtime.storageDir must be an absolute path, got %q" .Values.runtime.storageDir) }}
{{- end }}
{{- end }}

{{/*
Resolve the runtime socket path. Uses .Values.runtime.socketPath when set,
otherwise falls back to the per-runtime default.
*/}}
{{- define "snapshot.runtimeSocket" -}}
{{- if .Values.runtime.socketPath }}
{{- .Values.runtime.socketPath }}
{{- else if eq .Values.runtime.type "crio" }}
{{- "/var/run/crio/crio.sock" }}
{{- else }}
{{- "/run/containerd/containerd.sock" }}
{{- end }}
{{- end }}

{{/*
Host directory holding per-container storage (overlay upperdirs the agent
reads for rootfs-diff capture, and CRI-O config.json fallback).
*/}}
{{- define "snapshot.runtimeStorageDir" -}}
{{- if .Values.runtime.storageDir }}
{{- .Values.runtime.storageDir }}
{{- else if eq .Values.runtime.type "crio" }}
{{- "/var/lib/containers" }}
{{- else }}
{{- "/var/lib/containerd" }}
{{- end }}
{{- end }}

{{- define "snapshot.pageBrokerControlPath" -}}/pagebroker/control{{- end -}}
{{- define "snapshot.pageBrokerStagingPath" -}}/pagebroker/staging{{- end -}}

{{/* Driver access shared by the agent and PageBroker. */}}
{{- define "snapshot.nvidiaEnvironment" -}}
- name: NVIDIA_VISIBLE_DEVICES
  value: all
- name: NVIDIA_DRIVER_CAPABILITIES
  value: compute,utility
{{- end -}}

{{/* Remote agents must never run without the corresponding admission guard. */}}
{{- define "snapshot.provenanceChecks" -}}
{{- if .Values.provenance.consumer.enabled -}}
  {{- if not .Values.operator.enabled -}}{{- fail "provenance.consumer requires operator.enabled=true" -}}{{- end -}}
  {{- $_ := required "provenance.consumer.namespace is required" .Values.provenance.consumer.namespace -}}
  {{- if empty .Values.operator.artifactCleanup.remoteAgentNodes -}}{{- fail "provenance.consumer requires artifactCleanup.remoteAgentNodes" -}}{{- end -}}
{{- else if and .Values.operator.enabled .Values.operator.artifactCleanup.remoteAgentNodes -}}
  {{- fail "remoteAgentNodes requires provenance.consumer.enabled=true" -}}
{{- end -}}
{{- if .Values.provenance.provider.enabled -}}
  {{- if or .Values.operator.enabled .Values.provenance.consumer.enabled (not .Values.daemonset.enabled) (not .Values.daemonset.remoteControlPlane) -}}
    {{- fail "provenance.provider requires a remote provider agent without a local operator or consumer stamper" -}}
  {{- end -}}
  {{- $_ := required "provenance.provider.kubeconfigSecret is required" .Values.provenance.provider.kubeconfigSecret -}}
  {{- $_ := required "provenance.provider.tlsSecret is required" .Values.provenance.provider.tlsSecret -}}
  {{- $_ := required "provenance.provider.caBundle is required" .Values.provenance.provider.caBundle -}}
  {{- $_ := required "provenance.provider.reflectorUsername is required" .Values.provenance.provider.reflectorUsername -}}
{{- else if and .Values.daemonset.enabled .Values.daemonset.remoteControlPlane -}}
  {{- fail "remoteControlPlane requires provenance.provider.enabled=true" -}}
{{- end -}}
{{- end -}}
