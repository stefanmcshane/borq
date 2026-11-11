// Package managementbackup publishes and recovers independent native management snapshots.
package managementbackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

const (
	// MaxPayloadSize bounds encrypted native artifacts independently of large PostgreSQL objects.
	MaxPayloadSize int64 = 8 << 30
	// MaxPlaintextSize reserves space for authenticated age framing within the ciphertext limit.
	MaxPlaintextSize int64 = MaxPayloadSize - (8 << 20)
	// MaxConnections bounds saved cluster contexts in one SQLite artifact.
	MaxConnections = 10000
	// MaxConnectionSize bounds each self-contained kubeconfig before encoding.
	MaxConnectionSize = 1 << 20
	// MaxConnectionsSize bounds the complete encoded connection document.
	MaxConnectionsSize = 16 << 20
)

// Artifact is a complete native component snapshot, independent of other components' capture times.
type Artifact struct {
	// Version selects the native artifact protocol.
	Version int `json:"version"`
	// Recipient is the public age key used for this independently recoverable payload.
	Recipient string `json:"recipient"`
	// Kind is sqlite or openbao.
	Kind string `json:"kind"`
	// ID is immutable and unique within the source root.
	ID string `json:"id"`
	// CapturedAt is this component's actual snapshot start time.
	CapturedAt time.Time `json:"capturedAt"`
	// Size and SHA256 describe the completed payload, without secret material.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Entry binds safe completion metadata to captured S3 object identities.
type Entry struct {
	// Artifact is the verified completion marker.
	Artifact Artifact `json:"artifact"`
	// Payload and Completion preserve conditional-download identities.
	Payload    objectstore.Object `json:"payload"`
	Completion objectstore.Object `json:"completion"`
}

// NewID creates an immutable artifact identity without embedding credentials or timestamps in paths.
func NewID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("create management snapshot identity")
	}
	return hex.EncodeToString(value), nil
}

// ValidKind restricts native capture to the supported management components.
func ValidKind(kind string) bool { return kind == "sqlite" || kind == "openbao" }

// ManagedKey recognizes only reserved native payload and completion paths.
func ManagedKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 4 && parts[0] == "management" && ValidKind(parts[1]) && len(parts[2]) == 32 && strings.Trim(parts[2], "0123456789abcdef") == "" && (parts[3] == "payload" || parts[3] == "complete.json")
}

// PayloadKey is relative to the shared S3 root and preserves the immutable component identity.
func PayloadKey(artifact Artifact) string {
	return "management/" + artifact.Kind + "/" + artifact.ID + "/payload"
}

// CompletionKey names the marker published only after its payload was completely written.
func CompletionKey(artifact Artifact) string {
	return "management/" + artifact.Kind + "/" + artifact.ID + "/complete.json"
}

// Publish streams a completed private native file and publishes its completion marker last.
func Publish(ctx context.Context, client objectstore.Client, artifact Artifact, file string) (Artifact, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if artifact.Version != 1 || !ValidKind(artifact.Kind) || !ManagedKey(PayloadKey(artifact)) || artifact.CapturedAt.IsZero() {
		return Artifact{}, errors.New("invalid management snapshot identity")
	}
	if err := ValidateRecipient(artifact.Recipient); err != nil {
		return Artifact{}, err
	}
	directory, err := os.MkdirTemp("", "borq-native-encrypted-")
	if err != nil {
		return Artifact{}, errors.New("create private encryption workspace")
	}
	defer os.RemoveAll(directory)
	encrypted := filepath.Join(directory, "snapshot.age")
	if err := encryptFile(ctx, file, encrypted, artifact.Recipient); err != nil {
		return Artifact{}, err
	}
	opened, err := os.Open(encrypted)
	if err != nil {
		return Artifact{}, errors.New("open completed management snapshot")
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPayloadSize {
		return Artifact{}, errors.New("management snapshot payload has an unsupported size")
	}
	artifact.Size = info.Size()
	digest := sha256.New()
	if err := client.PutNew(ctx, PayloadKey(artifact), io.TeeReader(opened, digest), artifact.Size); err != nil {
		return Artifact{}, err
	}
	artifact.SHA256 = hex.EncodeToString(digest.Sum(nil))
	body, _ := json.Marshal(artifact)
	if err := client.PutNew(ctx, CompletionKey(artifact), strings.NewReader(string(body)), int64(len(body))); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// A failed response can still have committed the marker. Remove it before its payload.
		if cleanupErr := client.Delete(cleanup, CompletionKey(artifact)); cleanupErr != nil {
			return Artifact{}, errors.Join(err, cleanupErr)
		}
		return Artifact{}, errors.Join(err, client.Delete(cleanup, PayloadKey(artifact)))
	}
	return artifact, nil
}

// Inventory reads bounded completion markers and includes only their exact completed payloads.
// A marker with missing or mismatched data fails instead of claiming successful protection.
func Inventory(ctx context.Context, client objectstore.Client, objects []objectstore.Object) ([]Entry, error) {
	if len(objects) > 100000 {
		return nil, errors.New("native snapshot inventory exceeds 100000 objects")
	}
	inventory := map[string]objectstore.Object{}
	for _, object := range objects {
		if ManagedKey(object.Key) {
			if _, exists := inventory[object.Key]; exists {
				return nil, errors.New("duplicate native snapshot inventory key")
			}
			inventory[object.Key] = object
		}
	}
	metadataCount := 0
	metadataBytes := int64(0)
	for key, object := range inventory {
		if strings.HasSuffix(key, "/complete.json") {
			metadataCount++
			metadataBytes += object.Size
			if metadataCount > 50000 || metadataBytes > 32<<20 || object.Size <= 0 || object.Size > 64<<10 {
				return nil, errors.New("native snapshot completion metadata exceeds supported bounds")
			}
		}
	}
	result := []Entry{}
	for key, object := range inventory {
		if !strings.HasSuffix(key, "/complete.json") {
			continue
		}
		if len(result) >= 50000 || object.Size <= 0 || object.Size > 64<<10 {
			return nil, errors.New("native snapshot completion metadata exceeds supported bounds")
		}
		body, err := client.OpenCaptured(ctx, object)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(body, (64<<10)+1))
		closeErr := body.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) != object.Size {
			return nil, errors.New("native snapshot completion marker is truncated")
		}
		artifact, err := ParseCompletion(key, data)
		if err != nil {
			return nil, err
		}
		payload, exists := inventory[PayloadKey(artifact)]
		if !exists || payload.Size != artifact.Size || payload.ETag == "" {
			return nil, errors.New("completed native snapshot payload is missing or changed")
		}
		result = append(result, Entry{Artifact: artifact, Payload: payload, Completion: object})
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].Artifact, result[j].Artifact
		if a.CapturedAt.Equal(b.CapturedAt) {
			if a.ID == b.ID {
				return a.Kind < b.Kind
			}
			return a.ID < b.ID
		}
		return a.CapturedAt.Before(b.CapturedAt)
	})
	return result, nil
}

// ParseCompletion validates a marker's version, path, identity, length and content digest.
func ParseCompletion(key string, data []byte) (Artifact, error) {
	var artifact Artifact
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if len(data) > 64<<10 || decoder.Decode(&artifact) != nil || decoder.Decode(&struct{}{}) != io.EOF || artifact.Version != 1 || !ManagedKey(key) || CompletionKey(artifact) != key || artifact.CapturedAt.IsZero() || ValidateRecipient(artifact.Recipient) != nil || artifact.Size <= 0 || artifact.Size > MaxPayloadSize || len(artifact.SHA256) != 64 || strings.Trim(artifact.SHA256, "0123456789abcdef") != "" {
		return Artifact{}, errors.New("invalid native snapshot completion marker")
	}
	return artifact, nil
}

// List returns completed native snapshots without any dependency on the management database or vault.
func List(ctx context.Context, config objectstore.Config) ([]Entry, error) {
	client, err := objectstore.New(config)
	if err != nil {
		return nil, err
	}
	return listPrefix(ctx, client, "management/")
}

func listPrefix(ctx context.Context, client objectstore.Client, prefix string) ([]Entry, error) {
	objects := []objectstore.Object{}
	continuation := ""
	for {
		page, err := client.ListPage(ctx, prefix, continuation, 1000)
		if err != nil {
			return nil, err
		}
		objects = append(objects, page.Objects...)
		if len(objects) > 100000 {
			return nil, errors.New("native snapshot inventory exceeds 100000 objects")
		}
		if page.Continuation == "" {
			break
		}
		if continuation == page.Continuation {
			return nil, errors.New("native snapshot pagination did not advance")
		}
		continuation = page.Continuation
	}
	return Inventory(ctx, client, objects)
}

// Download verifies every payload byte before native restore can consume a private file.
func Download(ctx context.Context, config objectstore.Config, kind, id, destination string) (Artifact, error) {
	selected := Artifact{Kind: kind, ID: id}
	if !ManagedKey(PayloadKey(selected)) {
		return Artifact{}, errors.New("invalid native snapshot selection")
	}
	client, err := objectstore.New(config)
	if err != nil {
		return Artifact{}, err
	}
	entries, err := listPrefix(ctx, client, strings.TrimSuffix(PayloadKey(selected), "payload"))
	if err != nil {
		return Artifact{}, err
	}
	for _, entry := range entries {
		if entry.Artifact.Kind != kind || entry.Artifact.ID != id {
			continue
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return Artifact{}, errors.New("create private management recovery payload")
		}
		body, err := client.OpenCaptured(ctx, entry.Payload)
		if err != nil {
			output.Close()
			os.Remove(destination)
			return Artifact{}, err
		}
		digest := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(output, digest), io.LimitReader(body, entry.Artifact.Size+1))
		closeErr := errors.Join(body.Close(), output.Close())
		if copyErr != nil || closeErr != nil || size != entry.Artifact.Size || hex.EncodeToString(digest.Sum(nil)) != entry.Artifact.SHA256 {
			os.Remove(destination)
			return Artifact{}, errors.New("native management snapshot payload failed integrity validation")
		}
		return entry.Artifact, nil
	}
	return Artifact{}, errors.New("completed native snapshot was not found")
}

// Prune removes older completed artifacts of one component after a new success.
func Prune(ctx context.Context, config objectstore.Config, kind string, keepLast int) error {
	if !ValidKind(kind) || keepLast < 1 {
		return errors.New("invalid native snapshot retention")
	}
	client, err := objectstore.New(config)
	if err != nil {
		return err
	}
	objects, err := client.List(ctx, "management/"+kind+"/")
	if err != nil {
		return err
	}
	entries, err := Inventory(ctx, client, objects)
	if err != nil {
		return err
	}
	selected := []Entry{}
	for _, entry := range entries {
		if entry.Artifact.Kind == kind {
			selected = append(selected, entry)
		}
	}
	for _, entry := range selected[:max(0, len(selected)-keepLast)] {
		if err := client.Delete(ctx, entry.Completion.Key); err != nil {
			return err
		}
		if err := client.Delete(ctx, entry.Payload.Key); err != nil {
			return err
		}
	}
	markers := map[string]bool{}
	for _, object := range objects {
		markers[object.Key] = true
	}
	for _, object := range objects {
		if !ManagedKey(object.Key) || !strings.HasSuffix(object.Key, "/payload") || object.LastModified.IsZero() || object.LastModified.After(time.Now().Add(-24*time.Hour)) {
			continue
		}
		marker := strings.TrimSuffix(object.Key, "payload") + "complete.json"
		// Publication is bounded to 30 minutes; the grace period also protects offline rehydration.
		if !markers[marker] {
			if err := client.Delete(ctx, object.Key); err != nil {
				return err
			}
		}
	}
	return nil
}
