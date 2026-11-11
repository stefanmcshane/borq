package externalsecrets

import _ "embed"

//go:generate sh -c "curl -sL -o v2.4.0.yaml https://raw.githubusercontent.com/external-secrets/external-secrets/v2.4.0/deploy/crds/bundle.yaml"
//go:embed v2.4.0.yaml
var CRDYAML []byte
