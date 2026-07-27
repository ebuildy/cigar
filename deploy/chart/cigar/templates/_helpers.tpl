{{/*
Expand the name of the chart.
*/}}
{{- define "cigar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cigar.fullname" -}}
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
{{- define "cigar.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "cigar.labels" -}}
helm.sh/chart: {{ include "cigar.chart" . }}
{{ include "cigar.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cigar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cigar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Every secret the bot can consume, in a stable order.
*/}}
{{- define "cigar.knownSecretNames" -}}
GITLAB_TOKEN WEBHOOK_SECRET WEBHOOK_SIGNING_TOKEN COMMANDS_SIGNING_KEY
{{- end }}

{{/*
The subset the current configuration actually requires. Anything else is left
out of the Secret and out of the pod's environment, even if it is configured.
*/}}
{{- define "cigar.requiredSecretNames" -}}
{{- $names := list "GITLAB_TOKEN" }}
{{- $authMethod := .Values.config.webhook.authMethod | default "secret" }}
{{- if eq $authMethod "secret" }}
{{- $names = append $names "WEBHOOK_SECRET" }}
{{- end }}
{{- if eq $authMethod "signing_token" }}
{{- $names = append $names "WEBHOOK_SIGNING_TOKEN" }}
{{- end }}
{{- if .Values.config.commands.enabled }}
{{- $names = append $names "COMMANDS_SIGNING_KEY" }}
{{- end }}
{{- join " " $names }}
{{- end }}

{{/*
Name of the Secret object holding one secret. Takes (dict "root" $ "name" NAME).
Either a Secret the user manages, the per-secret Secret the External Secrets
Operator materialises, or the one this chart owns for literal values.
*/}}
{{- define "cigar.secretRefName" -}}
{{- $s := get .root.Values.secrets .name | default dict }}
{{- if $s.existingSecret }}
{{- $s.existingSecret }}
{{- else if $s.externalSecret }}
{{- include "cigar.externalSecretName" (dict "root" .root "ref" $s.externalSecret) }}
{{- else }}
{{- include "cigar.fullname" .root }}
{{- end }}
{{- end }}

{{/*
Name of the ExternalSecret (and its target Secret) rendered for one entry of
.Values.externalSecrets. Takes (dict "root" $ "ref" REF). Every secret
referencing that entry is a key of this one Secret.
*/}}
{{- define "cigar.externalSecretName" -}}
{{- printf "%s-%s" (include "cigar.fullname" .root) (.ref | lower | replace "_" "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Key to read inside that Secret. Takes (dict "root" $ "name" NAME). Only an
existing Secret can spell it differently; the ones the chart creates always use
the secret's own name.
*/}}
{{- define "cigar.secretRefKey" -}}
{{- $s := get .root.Values.secrets .name | default dict }}
{{- if $s.existingSecret }}
{{- $s.key | default .name }}
{{- else }}
{{- .name }}
{{- end }}
{{- end }}

{{/*
Reject secret configurations that would only fail once the pod is running.
Included by every template that consumes .Values.secrets.
*/}}
{{- define "cigar.validateSecrets" -}}
{{- $known := splitList " " (include "cigar.knownSecretNames" .) }}
{{- range $name, $s := .Values.secrets }}
{{- if not (has $name $known) }}
{{- fail (printf "secrets.%s is not a secret this chart knows — expected one of: %s" $name (join ", " $known)) }}
{{- end }}
{{- end }}
{{- range $name := splitList " " (include "cigar.requiredSecretNames" .) }}
{{- $s := get $.Values.secrets $name | default dict }}
{{- $es := $s.externalSecret | default "" }}
{{- $sources := list }}
{{- if $s.value }}
{{- $sources = append $sources "value" }}
{{- end }}
{{- if $s.existingSecret }}
{{- $sources = append $sources "existingSecret" }}
{{- end }}
{{- if $es }}
{{- $sources = append $sources "externalSecret" }}
{{- end }}
{{- if eq (len $sources) 0 }}
{{- fail (printf "secrets.%s needs a source: set value, existingSecret, or externalSecret — the current configuration requires %s" $name $name) }}
{{- end }}
{{- if gt (len $sources) 1 }}
{{- fail (printf "secrets.%s has more than one source (%s): set exactly one" $name (join ", " $sources)) }}
{{- end }}
{{- if $es }}
{{- $defined := $.Values.externalSecrets | default dict }}
{{- if not (hasKey $defined $es) }}
{{- fail (printf "secrets.%s.externalSecret is %q, which is not defined under externalSecrets — available: %s" $name $es (ternary "(none)" (join ", " (keys $defined | sortAlpha)) (eq (len $defined) 0))) }}
{{- end }}
{{- end }}
{{- /* GitLab signing tokens are whsec_ + standard base64 of a 32-byte key: 44
       base64 chars ending in a single "=" pad. The bot's decoder accepts any
       length, so a short key would only show up as silently weaker HMACs — and
       GitLab rejects a malformed token on the hook with a 422. Only checkable
       for a literal; the contents of an existing Secret are not visible here,
       and the generator path builds the value itself. */}}
{{- if and (eq $name "WEBHOOK_SIGNING_TOKEN") $s.value }}
{{- if not (regexMatch "^whsec_[A-Za-z0-9+/]{43}=$" $s.value) }}
{{- fail "secrets.WEBHOOK_SIGNING_TOKEN.value must be whsec_<base64> encoding a 32-byte key — generate one with: echo \"whsec_$(openssl rand -base64 32)\"" }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "cigar.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cigar.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
