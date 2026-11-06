{{/* The service account llm-router runs as. */}}
{{- define "llm-router.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (printf "%s-llm-router" .Release.Name) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* The service account gateways run as. */}}
{{- define "llm-router.gatewayServiceAccountName" -}}
{{- if .Values.gateway.serviceAccount.create -}}
{{- .Values.gateway.serviceAccount.name | default (printf "%s-llm-router-gateway" .Release.Name) -}}
{{- else -}}
{{- .Values.gateway.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* Anthropic workload identity: the token's volume, mount and variable. */}}
{{- define "llm-router.anthropicIdentityVolume" -}}
- name: anthropic-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          audience: {{ .Values.anthropicIdentity.audience | quote }}
          expirationSeconds: {{ .Values.anthropicIdentity.expirationSeconds }}
{{- end -}}
{{- define "llm-router.anthropicIdentityMount" -}}
- {name: anthropic-token, mountPath: /var/run/secrets/anthropic.com, readOnly: true}
{{- end -}}
{{- define "llm-router.anthropicIdentityEnv" -}}
- {name: ANTHROPIC_IDENTITY_TOKEN_FILE, value: /var/run/secrets/anthropic.com/token}
{{- if eq (include "llm-router.anthropicIdentitySource" .) "kubernetes" }}
- {name: ANTHROPIC_IDENTITY_TOKEN_SOURCE, value: kubernetes}
- {name: ANTHROPIC_IDENTITY_TOKEN_AUDIENCE, value: {{ .Values.anthropicIdentity.audience | quote }}}
- {name: LLMR_POD_NAME, valueFrom: {fieldRef: {fieldPath: metadata.name}}}
- {name: LLMR_POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
- {name: LLMR_POD_UID, valueFrom: {fieldRef: {fieldPath: metadata.uid}}}
- {name: LLMR_POD_SERVICE_ACCOUNT, valueFrom: {fieldRef: {fieldPath: spec.serviceAccountName}}}
{{- end }}
{{- end -}}

{{/* file or kubernetes; anything else is a mistake worth failing on. */}}
{{- define "llm-router.anthropicIdentitySource" -}}
{{- $s := .Values.anthropicIdentity.source | default "file" -}}
{{- if not (has $s (list "file" "kubernetes")) -}}
{{- fail (printf "anthropicIdentity.source must be file or kubernetes, not %q" $s) -}}
{{- end -}}
{{- $s -}}
{{- end -}}
