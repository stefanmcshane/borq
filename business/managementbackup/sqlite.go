package managementbackup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/client-go/tools/clientcmd"
)

// Connection contains a self-contained kubeconfig for exactly one saved cluster record.
type Connection struct {
	// Key selects the cluster document. @management selects the server's management connection.
	Key string `json:"key"`
	// Task selects an unfinished management task whose staged cluster connection must also survive recovery.
	Task string `json:"task,omitempty"`
	// Kubeconfig contains private inline credentials; it appears only inside encrypted payloads.
	Kubeconfig string `json:"kubeconfig"`
}

// BundleSQLite packages an already captured native database with the connections read from that same snapshot.
func BundleSQLite(ctx context.Context, snapshot string, connections []Connection, destination string) error {
	if err := sqlite.ValidateSnapshot(ctx, snapshot); err != nil {
		return err
	}
	if len(connections) > MaxConnections {
		return errors.New("saved cluster connections exceed native backup limits")
	}
	metadataBytes := 2
	for _, connection := range connections {
		if len(connection.Kubeconfig) > MaxConnectionSize || len(connection.Key) > 1024 || len(connection.Task) > 1024 {
			return errors.New("saved cluster connection exceeds native backup limits")
		}
		encoded, _ := json.Marshal(connection)
		metadataBytes += len(encoded) + 1
		if metadataBytes > MaxConnectionsSize {
			return errors.New("saved cluster connections exceed native backup limits")
		}
	}
	metadata, err := json.Marshal(connections)
	if err != nil || len(metadata) > MaxConnectionsSize || len(connections) > MaxConnections {
		return errors.New("saved cluster connections exceed native backup limits")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("create private SQLite backup bundle")
	}
	defer output.Close()
	input, err := os.Open(snapshot)
	if err != nil {
		return errors.New("open native SQLite snapshot")
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > MaxPlaintextSize-MaxConnectionsSize-4096 {
		return errors.New("native SQLite snapshot exceeds the encrypted payload limit")
	}
	writer := tar.NewWriter(output)
	if err := writer.WriteHeader(&tar.Header{Name: "borq.sqlite", Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatGNU}); err != nil {
		return err
	}
	if _, err := io.Copy(writer, contextReader{ctx: ctx, Reader: input}); err != nil {
		return errors.New("write SQLite snapshot bundle")
	}
	if err := writer.WriteHeader(&tar.Header{Name: "connections.json", Mode: 0600, Size: int64(len(metadata)), Typeflag: tar.TypeReg, Format: tar.FormatGNU}); err != nil {
		return err
	}
	if _, err := writer.Write(metadata); err != nil {
		return err
	}
	return errors.Join(writer.Close(), output.Sync())
}

// RestoreSQLite installs a native bundle into a fresh private data directory, retaining an explicit replay barrier.
func RestoreSQLite(ctx context.Context, bundle, destination string) error {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return errors.New("resolve SQLite recovery directory")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("SQLite recovery requires a fresh destination directory")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return errors.New("create SQLite recovery parent")
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".borq-recovery-")
	if err != nil {
		return errors.New("create private SQLite recovery directory")
	}
	defer os.RemoveAll(staging)
	input, err := os.Open(bundle)
	if err != nil {
		return errors.New("open SQLite recovery bundle")
	}
	defer input.Close()
	reader := tar.NewReader(input)
	header, err := reader.Next()
	if err != nil || header.Name != "borq.sqlite" || header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Size <= 0 || header.Size > MaxPlaintextSize-MaxConnectionsSize-4096 || len(header.PAXRecords) != 0 {
		return errors.New("invalid SQLite snapshot bundle header")
	}
	file, err := os.OpenFile(filepath.Join(staging, "borq.sqlite"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("create recovered SQLite file")
	}
	count, copyErr := io.Copy(file, contextReader{ctx: ctx, Reader: reader})
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || count != header.Size {
		return errors.New("SQLite snapshot bundle is truncated")
	}
	header, err = reader.Next()
	if err != nil || header.Name != "connections.json" || header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Size < 0 || header.Size > MaxConnectionsSize || len(header.PAXRecords) != 0 {
		return errors.New("invalid saved connection bundle")
	}
	var connections []Connection
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&connections) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(connections) > MaxConnections {
		return errors.New("invalid saved connection metadata")
	}
	if _, err := reader.Next(); err != io.EOF {
		return errors.New("unexpected SQLite bundle entries")
	}
	trailing, err := io.ReadAll(io.LimitReader(input, 1))
	if err != nil || len(trailing) != 0 {
		return errors.New("unexpected SQLite bundle trailing data")
	}
	if err := sqlite.ValidateSnapshot(ctx, filepath.Join(staging, "borq.sqlite")); err != nil {
		return err
	}
	if err := installConnections(ctx, staging, destination, connections); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "RECOVERY_REQUIRED"), []byte("Native SQLite recovery. Review queued operations and rebind storage/vault/cluster connections before explicit resume.\n"), 0600); err != nil {
		return errors.New("write recovery replay barrier")
	}
	if err := os.Rename(staging, destination); err != nil {
		return errors.New("publish recovered SQLite directory")
	}
	return nil
}
func installConnections(ctx context.Context, staging, destination string, connections []Connection) error {
	database, err := sqlite.New(ctx, filepath.Join(staging, "borq.sqlite"))
	if err != nil {
		return err
	}
	defer database.Close()
	records, err := database.ListBounded(ctx, "cluster", MaxConnections, 32<<20)
	if err != nil {
		return err
	}
	documents := map[string]sqlite.Record{}
	for _, record := range records {
		documents[record.Key] = record
	}
	seen := map[string]bool{}
	writes := []sqlite.Write{}
	tasks := map[string]sqlite.Record{}
	taskValues := map[string]map[string]any{}
	taskRecords, err := database.ListBounded(ctx, "management-task", MaxConnections, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range taskRecords {
		tasks[record.Key] = record
	}
	for _, connection := range connections {
		identity := connection.Task + "\x00" + connection.Key
		if seen[identity] || len(connection.Kubeconfig) > MaxConnectionSize || len(connection.Key) > 1024 || len(connection.Task) > 1024 {
			return errors.New("duplicate or oversized saved connection")
		}
		seen[identity] = true
		config, err := clientcmd.Load([]byte(connection.Kubeconfig))
		if err != nil || clientcmd.Validate(*config) != nil {
			return errors.New("saved recovery connection is invalid")
		}
		for _, cluster := range config.Clusters {
			if cluster.CertificateAuthority != "" {
				return errors.New("recovery connection depends on an external certificate")
			}
		}
		for _, auth := range config.AuthInfos {
			if auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" {
				return errors.New("recovery connection is not self-contained")
			}
		}
		digest := sha256.Sum256([]byte(identity))
		name := hex.EncodeToString(digest[:16]) + ".yaml"
		if connection.Key == "@management" && connection.Task == "" {
			name = "management.yaml"
		}
		if err := os.MkdirAll(filepath.Join(staging, "clusters"), 0700); err != nil {
			return errors.New("create saved recovery connections")
		}
		if err := os.WriteFile(filepath.Join(staging, "clusters", name), []byte(connection.Kubeconfig), 0600); err != nil {
			return errors.New("write saved recovery connection")
		}
		if connection.Key == "@management" && connection.Task == "" {
			continue
		}
		if connection.Task != "" {
			if _, decoded := taskValues[connection.Task]; !decoded {
				record, exists := tasks[connection.Task]
				if !exists {
					return errors.New("saved connection has no matching management task")
				}
				var value map[string]any
				if json.Unmarshal(record.Value, &value) != nil {
					return errors.New("invalid saved management task")
				}
				taskValues[connection.Task] = value
			}
			configuration, _ := taskValues[connection.Task]["configuration"].(map[string]any)
			organizations, _ := configuration["organizations"].([]any)
			matched := false
			for _, item := range organizations {
				organization, _ := item.(map[string]any)
				organizationName, _ := organization["name"].(string)
				clusters, _ := organization["clusters"].([]any)
				for _, item := range clusters {
					cluster, _ := item.(map[string]any)
					clusterName, _ := cluster["name"].(string)
					if organizationName != "" && clusterName != "" && organizationName+"/"+clusterName == connection.Key {
						cluster["kubeconfigPath"], cluster["kubeconfig"], cluster["inCluster"] = filepath.Join(destination, "clusters", name), "", false
						matched = true
					}
				}
			}
			if !matched {
				return errors.New("saved task connection has no matching cluster")
			}
			continue
		}
		record, exists := documents[connection.Key]
		if !exists {
			return errors.New("saved connection has no matching cluster record")
		}
		var value map[string]any
		if json.Unmarshal(record.Value, &value) != nil {
			return errors.New("invalid saved cluster record")
		}
		value["kubeconfigPath"] = filepath.Join(destination, "clusters", name)
		value["kubeconfig"] = ""
		value["inCluster"] = false
		data, _ := json.Marshal(value)
		writes = append(writes, sqlite.Write{Collection: "cluster", Key: record.Key, ExpectedRevision: record.Revision, Value: data})
	}
	for key := range documents {
		if !seen["\x00"+key] {
			return errors.New("SQLite snapshot is missing a saved cluster connection")
		}
	}
	for key, value := range taskValues {
		record := tasks[key]
		data, _ := json.Marshal(value)
		writes = append(writes, sqlite.Write{Collection: "management-task", Key: key, ExpectedRevision: record.Revision, Value: data})
	}
	return database.Commit(ctx, writes)
}
