package appapi

import "embed"

// openapiDocument is embedded so deployed servers document the exact generated
// contract without depending on files in the process working directory.
//
//go:embed openapi.yaml
var openapiDocument []byte

// documentationFiles must be embedded at package scope so the API explorer and
// its pinned Swagger UI assets are available without runtime files or a CDN.
//
//go:embed api-docs/*
var documentationFiles embed.FS
