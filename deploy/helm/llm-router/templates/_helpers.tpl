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
{{- end -}}
