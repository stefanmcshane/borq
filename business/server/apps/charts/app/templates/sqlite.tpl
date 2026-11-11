{{- define "sqlite.volumes" -}}
- name: borq-sqlite
  persistentVolumeClaim:
    claimName: {{ .sqlite.volume | quote }}
- name: borq-sqlite-recovery
  configMap:
    name: {{ .sqlite.recoveryConfig | quote }}
- name: borq-sqlite-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          expirationSeconds: 600
      - configMap:
          name: kube-root-ca.crt
          items:
            - key: ca.crt
              path: ca.crt
{{- if .sqlite.backups }}
- name: borq-litestream-config
  configMap:
    name: {{ .sqlite.config | quote }}
{{- end }}
{{- end -}}

{{- define "sqlite.mount" -}}
- name: borq-sqlite
  mountPath: {{ .sqlite.directory | quote }}
{{- end -}}

{{- define "sqlite.init" -}}
{{- if .sqlite.backups }}
- name: sqlite-restore
  image: {{ .sqlite.image | quote }}
  imagePullPolicy: Always
  command: ["/bin/sh", "-ec"]
  args:
    - |
      if [ ! -f {{ .sqlite.path | quote }} ]; then
        rm -f {{ printf "%s.restoring" .sqlite.path | quote }} {{ printf "%s.restoring-wal" .sqlite.path | quote }} {{ printf "%s.restoring-shm" .sqlite.path | quote }}
        litestream restore -config /etc/litestream/litestream.yml -if-replica-exists -o {{ printf "%s.restoring" .sqlite.path | quote }} {{ .sqlite.path | quote }}
      fi
  envFrom:
    - secretRef:
        name: {{ .sqlite.secret | quote }}
  volumeMounts:
    {{- include "sqlite.mount" . | nindent 4 }}
    - name: borq-litestream-config
      mountPath: /etc/litestream
      readOnly: true
{{- end }}
- name: sqlite-prepare
  image: {{ .sqlite.recoveryImage | quote }}
  imagePullPolicy: Always
  command: ["python", "/etc/borq-sqlite/sqlite-prepare.py"]
  args: ["prepare", {{ .sqlite.path | quote }}, {{ .namespace | quote }}, {{ .sqlite.state | quote }}]
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 128Mi}
  volumeMounts:
    {{- include "sqlite.mount" . | nindent 4 }}
    - name: borq-sqlite-recovery
      mountPath: /etc/borq-sqlite
      readOnly: true
    - name: borq-sqlite-token
      mountPath: /var/run/borq-sqlite
      readOnly: true
{{- end -}}

{{- define "sqlite.replication" -}}
{{ toYaml (list .sqlite.replication) }}
{{- end -}}
