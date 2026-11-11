package apps

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/stefanmcshane/borq/business/server/platform"
	"golang.org/x/crypto/bcrypt"
)

func managementRequestDigest(configuration ManagementConfiguration) ([sha256.Size]byte, error) {
	// Staging assigns cluster ownership, so retries must use the same values.
	configuration.Organizations = slices.Clone(configuration.Organizations)
	for index := range configuration.Organizations {
		organization := &configuration.Organizations[index]
		organization.Clusters = slices.Clone(organization.Clusters)
		for clusterIndex := range organization.Clusters {
			cluster := &organization.Clusters[clusterIndex]
			cluster.Organization = organization.Name
			cluster.ID = platform.ClusterID(organization.Name, cluster.Name)
		}
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("%w: configuration cannot be encoded", ErrInvalid)
	}
	// The fixed-length digest avoids bcrypt's input limit. Only its salted, slow
	// hash is persisted: a plain digest would enable cheap guesses of inline secrets.
	return sha256.Sum256(encoded), nil
}

func (task managementTask) matchesRequest(digest [sha256.Size]byte) (bool, error) {
	if task.RequestHash == "" {
		// Never restage a legacy task to compare it: staging can rotate credentials
		// or capture newer ownership revisions while the original worker still runs.
		previous, err := managementRequestDigest(task.Configuration)
		return previous == digest, err
	}
	err := bcrypt.CompareHashAndPassword([]byte(task.RequestHash), digest[:])
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return err == nil, err
}

type managementOperationConflict struct {
	ID     string
	Legacy bool
}

func (err managementOperationConflict) Error() string {
	if err.Legacy {
		return fmt.Sprintf("management operation %s is still active; its stored configuration cannot be matched to this request; wait for it to finish before reapplying", err.ID)
	}
	return fmt.Sprintf("management operation %s is still active with a different configuration; wait for it to finish before applying changes", err.ID)
}

func (err managementOperationConflict) Unwrap() error { return ErrConflict }

type managementNetworkConflict struct{}

func (err managementNetworkConflict) Error() string {
	return "networking differs from bootstrap; rerun management bootstrap with these network settings"
}

func (err managementNetworkConflict) Unwrap() error { return ErrConflict }

// ManagementRequestMessage exposes only known management conflict guidance,
// ignoring surrounding diagnostics which may contain credentials.
func ManagementRequestMessage(err error) string {
	var operation managementOperationConflict
	if errors.As(err, &operation) {
		return operation.Error()
	}
	if message := OrganizationOidcRequestMessage(err); message != "" {
		return message
	}
	var network managementNetworkConflict
	if errors.As(err, &network) {
		return network.Error()
	}
	return ""
}
