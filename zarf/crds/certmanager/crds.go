package certmanager

import _ "embed"

//go:generate sh -c "curl -sL -o v1.20.2.yaml https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.crds.yaml"
//go:embed v1.20.2.yaml
var CRDYAML []byte
