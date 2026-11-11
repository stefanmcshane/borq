package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/stefanmcshane/borq/business/server/platform"
)

func readNetworkSettings(path, clusterName string) (platform.NetworkSettings, error) {
	file, err := os.Open(path)
	if err != nil {
		return platform.NetworkSettings{}, errors.New("read bootstrap network settings")
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(encoded) > 64*1024 {
		return platform.NetworkSettings{}, errors.New("bootstrap network settings exceed size limit or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var settings platform.NetworkSettings
	if err := decoder.Decode(&settings); err != nil {
		return settings, errors.New("invalid bootstrap network settings")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return settings, errors.New("network settings must contain one JSON object")
	}
	if settings.Tailscale == nil && settings.Cloudflare == nil {
		return settings, errors.New("mounted network settings must configure a provider; restore the installed configuration")
	}
	return platform.NormalizeNetworkSettings(settings, clusterName)
}
