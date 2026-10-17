{{- /*
  go-licenses report template for the Go library index in THIRD_PARTY_NOTICES/
  README.txt (see the Makefile's notices target). .Name is a library's import
  path, which can sit below its module's root; .Version is that module's version.
  go-licenses emits one row per licence it identifies, so a library under several
  licences gets several lines.
*/ -}}
{{- range . }}
  {{ .Name }}{{ if ne .Version "Unknown" }} {{ .Version }}{{ end }} ({{ .LicenseName }})
{{- end }}
