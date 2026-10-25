package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	openbaoapi "github.com/openbao/openbao/api/v2"
)

// DatabaseConnectionInput configures a PostgreSQL connection without retaining its bootstrap password.
type DatabaseConnectionInput struct {
	// ConnectionUrl is a PostgreSQL URI containing username and password templates.
	ConnectionUrl string
	// Username identifies the dedicated OpenBao control account.
	Username string
	// BootstrapPassword is supplied only when enrolling a new connection.
	BootstrapPassword string `json:"-"`
	// AllowedRole restricts the connection to its application static role.
	AllowedRole string
	// RootCaPath names the trusted public CA file inside the OpenBao server.
	RootCaPath string
}

// DatabaseConnection contains the non-secret connection identity returned by OpenBao.
type DatabaseConnection struct {
	// Username identifies the connection's control account.
	Username string
	// ConnectionUrl is the configured template, never a resolved password.
	ConnectionUrl string
	// AllowedRole identifies the application role permitted to use this connection.
	AllowedRole string
}

// StaticRole maps one existing PostgreSQL login to automatic password rotation.
type StaticRole struct {
	// Connection is the database connection name within the engine mount.
	Connection string
	// Username is the stable PostgreSQL application login.
	Username string
	// RotationPeriod is the interval between automatic password changes.
	RotationPeriod time.Duration
}

// StaticCredentials contains the current application credential and its rotation metadata.
type StaticCredentials struct {
	// Username is the stable database login.
	Username string
	// Password is sensitive transient material and must never be serialized.
	Password string `json:"-"`
	// LastRotation is OpenBao's last successful password change.
	LastRotation time.Time
	// RotationPeriod is the configured password lifetime.
	RotationPeriod time.Duration
}

// EnsureDatabaseEngine enables an isolated database mount without modifying existing engines.
func (c *Client) EnsureDatabaseEngine(ctx context.Context, mount string) error {
	if _, err := databasePath(mount, "check", "entry"); err != nil {
		return err
	}
	mounts, err := c.cli.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return databaseRequestError(ctx, "list mounts", err)
	}
	if existing, ok := mounts[mount+"/"]; ok {
		if existing.Type != "database" {
			return errors.New("openbao mount must use the database engine")
		}
		return nil
	}
	if err := c.cli.Sys().MountWithContext(ctx, mount, &openbaoapi.MountInput{Type: "database"}); err != nil {
		return databaseRequestError(ctx, "enable engine", err)
	}
	return nil
}

// ReadDatabaseConnection retrieves the connection's public configuration.
func (c *Client) ReadDatabaseConnection(ctx context.Context, mount, name string) (DatabaseConnection, error) {
	data, err := c.readDatabase(ctx, mount, "config", name)
	if err != nil {
		return DatabaseConnection{}, err
	}
	details, ok := data["connection_details"].(map[string]any)
	if !ok {
		return DatabaseConnection{}, errors.New("openbao database connection details are invalid")
	}
	username, _ := details["username"].(string)
	connection, _ := details["connection_url"].(string)
	roles, err := stringList(data["allowed_roles"])
	if err != nil || len(roles) != 1 || username == "" || connection == "" {
		return DatabaseConnection{}, errors.New("openbao database connection identity is invalid")
	}
	return DatabaseConnection{Username: username, ConnectionUrl: connection, AllowedRole: roles[0]}, nil
}

// ConfigureDatabaseConnection enrolls a connection or updates routing while preserving its stored root password.
func (c *Client) ConfigureDatabaseConnection(ctx context.Context, mount, name string, input DatabaseConnectionInput) error {
	address, err := url.Parse(strings.NewReplacer("{{username}}", "control", "{{password}}", "credential").Replace(input.ConnectionUrl))
	if err != nil || address.Scheme != "postgresql" || address.Hostname() == "" || input.Username == "" || input.AllowedRole == "" || !strings.HasPrefix(input.RootCaPath, "/") || !strings.Contains(input.ConnectionUrl, "{{username}}") || !strings.Contains(input.ConnectionUrl, "{{password}}") {
		return errors.New("openbao PostgreSQL connection requires a templated URI, control account, role, and absolute CA path")
	}
	query := address.Query()
	if query.Get("sslmode") != "verify-full" || query.Get("sslrootcert") != input.RootCaPath {
		return errors.New("openbao PostgreSQL connection must verify the server hostname and configured CA")
	}
	existing, readErr := c.ReadDatabaseConnection(ctx, mount, name)
	if readErr != nil && !errors.Is(readErr, ErrNotFound) {
		return readErr
	}
	if readErr == nil && existing.Username != input.Username {
		return errors.New("openbao database connection control account changed")
	}
	if readErr == nil && existing.ConnectionUrl == input.ConnectionUrl && existing.AllowedRole == input.AllowedRole {
		return nil
	}
	data := map[string]any{"plugin_name": "postgresql-database-plugin", "connection_url": input.ConnectionUrl, "username": input.Username, "allowed_roles": []string{input.AllowedRole}, "password_authentication": "scram-sha-256", "verify_connection": true, "max_connection_lifetime": "1m"}
	if errors.Is(readErr, ErrNotFound) {
		if input.BootstrapPassword == "" {
			return errors.New("openbao database enrollment requires a bootstrap password")
		}
		data["password"] = input.BootstrapPassword
	}
	return c.writeDatabase(ctx, mount, "config", name, data)
}

// ReadStaticRole retrieves an existing static role policy without reading credentials.
func (c *Client) ReadStaticRole(ctx context.Context, mount, name string) (StaticRole, error) {
	data, err := c.readDatabase(ctx, mount, "static-roles", name)
	if err != nil {
		return StaticRole{}, err
	}
	connection, _ := data["db_name"].(string)
	username, _ := data["username"].(string)
	period, err := databaseDuration(data["rotation_period"])
	if err != nil || period < time.Minute || connection == "" || username == "" {
		return StaticRole{}, errors.New("openbao static role policy is invalid")
	}
	return StaticRole{Connection: connection, Username: username, RotationPeriod: period}, nil
}

// PutStaticRole creates or updates automatic rotation for one stable application login.
func (c *Client) PutStaticRole(ctx context.Context, mount, name string, input StaticRole) error {
	if input.Connection == "" || input.Username == "" || input.RotationPeriod < time.Minute || input.RotationPeriod%time.Second != 0 {
		return errors.New("openbao static role requires a connection, username, and rotation period of at least one minute")
	}
	return c.writeDatabase(ctx, mount, "static-roles", name, map[string]any{"db_name": input.Connection, "username": input.Username, "rotation_period": int64(input.RotationPeriod / time.Second)})
}

// ReadStaticCredentials retrieves the current password without creating a lease or changing it.
func (c *Client) ReadStaticCredentials(ctx context.Context, mount, name string) (StaticCredentials, error) {
	data, err := c.readDatabase(ctx, mount, "static-creds", name)
	if err != nil {
		return StaticCredentials{}, err
	}
	username, _ := data["username"].(string)
	password, _ := data["password"].(string)
	last, _ := data["last_vault_rotation"].(string)
	rotation, parseErr := time.Parse(time.RFC3339Nano, last)
	period, periodErr := databaseDuration(data["rotation_period"])
	if username == "" || password == "" || parseErr != nil || periodErr != nil || period < time.Minute {
		return StaticCredentials{}, errors.New("openbao static credentials are incomplete")
	}
	return StaticCredentials{Username: username, Password: password, LastRotation: rotation, RotationPeriod: period}, nil
}

// RotateDatabaseRoot changes the dedicated control password, which remains inside OpenBao.
func (c *Client) RotateDatabaseRoot(ctx context.Context, mount, name string) error {
	return c.writeDatabase(ctx, mount, "rotate-root", name, map[string]any{})
}

// RotateStaticRole changes the application password and resets its automatic rotation interval.
func (c *Client) RotateStaticRole(ctx context.Context, mount, name string) error {
	return c.writeDatabase(ctx, mount, "rotate-role", name, map[string]any{})
}

// DeleteStaticRole stops automatic rotation without dropping the externally owned SQL role.
func (c *Client) DeleteStaticRole(ctx context.Context, mount, name string) error {
	return c.deleteDatabase(ctx, mount, "static-roles", name)
}

// DeleteDatabaseConnection removes a deleted database's stored control credential.
func (c *Client) DeleteDatabaseConnection(ctx context.Context, mount, name string) error {
	return c.deleteDatabase(ctx, mount, "config", name)
}

func (c *Client) readDatabase(ctx context.Context, mount, collection, name string) (map[string]any, error) {
	path, err := databasePath(mount, collection, name)
	if err != nil {
		return nil, err
	}
	secret, err := c.cli.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, databaseRequestError(ctx, "read "+collection, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, ErrNotFound
	}
	return secret.Data, nil
}

func (c *Client) writeDatabase(ctx context.Context, mount, collection, name string, data map[string]any) error {
	path, err := databasePath(mount, collection, name)
	if err != nil {
		return err
	}
	if _, err := c.cli.Logical().WriteWithContext(ctx, path, data); err != nil {
		return databaseRequestError(ctx, "write "+collection, err)
	}
	return nil
}

func (c *Client) deleteDatabase(ctx context.Context, mount, collection, name string) error {
	path, err := databasePath(mount, collection, name)
	if err != nil {
		return err
	}
	if _, err := c.cli.Logical().DeleteWithContext(ctx, path); err != nil {
		if errors.Is(databaseRequestError(ctx, "delete "+collection, err), ErrNotFound) {
			return nil
		}
		return databaseRequestError(ctx, "delete "+collection, err)
	}
	return nil
}

func databasePath(mount, collection, name string) (string, error) {
	for _, part := range []string{mount, collection, name} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\?#%\x00") {
			return "", errors.New("openbao database paths require individual nonempty segments")
		}
	}
	return mount + "/" + collection + "/" + name, nil
}

func databaseDuration(value any) (time.Duration, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	var number json.Number
	if err := json.Unmarshal(encoded, &number); err != nil {
		return 0, err
	}
	seconds, err := number.Int64()
	if err != nil || seconds < 0 || seconds > int64((time.Duration(1<<63-1))/time.Second) {
		return 0, errors.New("invalid database duration")
	}
	return time.Duration(seconds) * time.Second, nil
}

func stringList(value any) ([]string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var values []string
	err = json.Unmarshal(encoded, &values)
	return values, err
}

func databaseRequestError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("openbao database %s: %w", operation, ctx.Err())
	}
	var response *openbaoapi.ResponseError
	if errors.As(err, &response) {
		if response.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("openbao database %s failed with status %d", operation, response.StatusCode)
	}
	return fmt.Errorf("openbao database %s request failed", operation)
}
