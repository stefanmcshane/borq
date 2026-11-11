package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type bootstrapRecovery struct {
	Borq struct {
		AdminToken string `json:"adminToken"`
	} `json:"borq"`
	OpenBao managementbackup.OpenBaoRecovery `json:"openbao"`
}

// ExportKeysInput selects explicit Kubernetes access and the private output file.
type ExportKeysInput struct {
	// KubeconfigPath identifies the management cluster without a Borq login.
	KubeconfigPath string
	// OutputFile receives both administrator credentials and the vault recovery material.
	OutputFile string
	// Timeout bounds waiting for the server to persist its credentials.
	Timeout time.Duration
}

// ExportKeys reads existing cluster credentials without creating or rotating keys.
func ExportKeys(ctx context.Context, input ExportKeysInput) (string, error) {
	if input.KubeconfigPath == "" || input.OutputFile == "" || input.Timeout <= 0 {
		return "", errors.New("management keys requires a kubeconfig, output file and positive timeout")
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	timeoutError := errors.New("timed out waiting for management keys; let bootstrap finish, then run borq management keys again. If setup has stalled, check kubectl -n borq-mgmt get pods")
	if _, err := connection.Namespaces().Get(ctx, defaultNamespace); err != nil {
		if ctx.Err() != nil {
			return "", timeoutError
		}
		if apierrors.IsNotFound(err) {
			return "", errors.New("Borq is not installed in the selected Kubernetes context yet; run borq management bootstrap, or let an ongoing bootstrap finish, then run borq management keys again")
		}
		return "", errors.New("cannot access the management namespace; check the selected Kubernetes context and your cluster permissions")
	}
	for {
		material, ready, err := readManagementKeys(ctx, connection)
		if err != nil && ctx.Err() == nil {
			return "", err
		}
		if ready && ctx.Err() == nil {
			return saveBootstrapRecovery(input.OutputFile, material)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", timeoutError
		case <-timer.C:
		}
	}
}

func readManagementKeys(ctx context.Context, connection kubernetes.Connection) (bootstrapRecovery, bool, error) {
	var material bootstrapRecovery
	admin, err := connection.Secrets().Get(ctx, defaultNamespace, "borq-admin-auth")
	if apierrors.IsNotFound(err) {
		return material, false, nil
	}
	if err != nil {
		return material, false, errors.New("read Borq administrator Secret; check management cluster access")
	}
	material.Borq.AdminToken = admin.Data["token"]
	if len(material.Borq.AdminToken) < 32 || len(material.Borq.AdminToken) > 4096 || strings.ContainsAny(material.Borq.AdminToken, "\r\n\t ") {
		return material, false, errors.New("Borq administrator Secret contains an invalid token")
	}
	secret, err := connection.Secrets().Get(ctx, defaultNamespace, "borq-openbao-admin")
	if apierrors.IsNotFound(err) {
		return material, false, nil
	}
	if err != nil {
		return material, false, errors.New("read OpenBao administrator Secret; check management cluster access")
	}
	material.OpenBao.OriginalToken = secret.Data["token"]
	material.OpenBao.SealKey = secret.Data["sealKey"]
	key, err := base64.StdEncoding.DecodeString(material.OpenBao.SealKey)
	if err != nil || len(key) != 32 {
		return material, false, errors.New("OpenBao Secret contains an invalid auto-unseal key; refusing to export incomplete recovery material")
	}
	if len(material.OpenBao.OriginalToken) != 64 || strings.Trim(material.OpenBao.OriginalToken, "0123456789abcdef") != "" {
		return material, false, errors.New("OpenBao Secret contains an invalid administrator token")
	}
	return material, true, nil
}

// ReadAdminToken reads the Borq credential from an independently retained private keys file.
func ReadAdminToken(file string) (string, error) {
	var keys bootstrapRecovery
	if err := offsite.ReadPrivateConfig(file, &keys); err != nil {
		return "", err
	}
	token := keys.Borq.AdminToken
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("management keys file contains an invalid Borq administrator token")
	}
	return token, nil
}

func saveBootstrapRecovery(destination string, recovery bootstrapRecovery) (string, error) {
	path, err := filepath.Abs(destination)
	if err != nil {
		return "", errors.New("find local recovery file location")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", fmt.Errorf("existing recovery file must be a private regular file at %s", path)
		}
		var previous bootstrapRecovery
		readErr := offsite.ReadPrivateConfig(path, &previous)
		if readErr != nil || previous.Borq.AdminToken != recovery.Borq.AdminToken || previous.OpenBao.OriginalToken != recovery.OpenBao.OriginalToken || previous.OpenBao.SealKey != recovery.OpenBao.SealKey || !slices.Equal(previous.OpenBao.UnsealKeys, recovery.OpenBao.UnsealKeys) {
			return "", fmt.Errorf("existing recovery file at %s is incomplete or does not match this installation; choose a new --output-file location", path)
		}
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("create private recovery file at %s: %w", path, err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(recovery)
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("save private recovery file at %s; keys remain in the cluster, rerun management keys to retry", path)
	}
	return path, nil
}
