package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/text/secure/precis"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RetireBootstrapCredentials removes CNPG password ownership before OpenBao changes the application password.
func (client Client) RetireBootstrapCredentials(ctx context.Context, input Input) error {
	if err := client.retireLegacyEnvironment(ctx, input); err != nil {
		return err
	}
	handle := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace)
	cluster, err := handle.Get(ctx, input.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cluster.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return errors.New("PostgreSQL cluster ownership conflict")
	}
	bootstrap, _, err := unstructured.NestedMap(cluster.Object, "spec", "bootstrap")
	if err != nil {
		return errors.New("PostgreSQL bootstrap configuration is invalid")
	}
	explicit := false
	for _, method := range []string{"initdb", "recovery"} {
		secret, _, _ := unstructured.NestedString(bootstrap, method, "secret", "name")
		owner, _, _ := unstructured.NestedString(bootstrap, method, "owner")
		if secret == input.Name+"-app" && owner == input.Username {
			explicit = true
		}
	}
	if !explicit {
		return errors.New("PostgreSQL bootstrap must retain its explicit application credential reference")
	}
	roles, found, err := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
	if err != nil {
		return errors.New("PostgreSQL managed role configuration is invalid")
	}
	remaining := make([]any, 0, len(roles))
	for _, entry := range roles {
		role, ok := entry.(map[string]any)
		if !ok {
			return errors.New("PostgreSQL managed role configuration is invalid")
		}
		if role["name"] == input.Username {
			secret, _, _ := unstructured.NestedString(role, "passwordSecret", "name")
			if secret != input.Name+"-app" {
				return errors.New("PostgreSQL application role has an unexpected password owner")
			}
			continue
		}
		remaining = append(remaining, entry)
	}
	if found && len(remaining) != len(roles) {
		if err := unstructured.SetNestedSlice(cluster.Object, remaining, "spec", "managed", "roles"); err != nil {
			return err
		}
		if _, err := handle.Update(ctx, cluster, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return client.deleteCredentialSecret(ctx, input.Namespace, input.Name+"-app", input.Owner)
}

func (client Client) deleteCredentialSecret(ctx context.Context, namespace, name, owner string) error {
	secretHandle := client.kube.CoreV1().Secrets(namespace)
	secret, err := secretHandle.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Labels["borq.run/postgres-id"] != owner {
		return errors.New("PostgreSQL bootstrap credential ownership conflict")
	}
	uid := secret.UID
	err = secretHandle.Delete(ctx, secret.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (client Client) retireLegacyEnvironment(ctx context.Context, input Input) error {
	if credentialNamespace(input) == input.Namespace {
		return nil
	}
	if err := client.deleteOne(ctx, "PushSecret", input.Namespace, input.Name+"-environment", input.Owner); err != nil {
		return err
	}
	_, err := client.dynamic.Resource(resource("PushSecret")).Namespace(input.Namespace).Get(ctx, input.Name+"-environment", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("waiting for the legacy PostgreSQL environment producer to stop")
}

// BootstrapManagementRole enrolls the destination-local control login using a SCRAM verifier over stdin.
func (client Client) BootstrapManagementRole(ctx context.Context, input Input, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(input.Username) || input.Username == "postgres" || input.Username == "borq_openbao" || password == "" {
		return errors.New("PostgreSQL control enrollment requires a non-reserved application username and password")
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return errors.New("PostgreSQL control credential could not be prepared")
	}
	primary, err := client.primary(ctx, input.Namespace, input.Name)
	if err != nil {
		return err
	}
	query := managementRoleSql(input.Username, verifier)
	_, err = client.execInput(ctx, input.Namespace, primary, []string{"psql", "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname", "postgres"}, strings.NewReader(query))
	return err
}

func managementRoleSql(username, verifier string) string {
	return fmt.Sprintf(`SET log_statement = 'none';
SET log_min_error_statement = 'panic';
DO $borq$ BEGIN
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'borq_openbao') THEN
CREATE ROLE borq_openbao;
END IF;
END $borq$;
ALTER ROLE borq_openbao LOGIN NOSUPERUSER NOCREATEDB CREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD '%s';
DO $borq$ DECLARE membership record; BEGIN
FOR membership IN SELECT role.rolname FROM pg_auth_members member JOIN pg_roles role ON role.oid = member.roleid JOIN pg_roles control ON control.oid = member.member WHERE control.rolname = 'borq_openbao' LOOP
EXECUTE format('REVOKE %%I FROM borq_openbao', membership.rolname);
END LOOP;
IF current_setting('server_version_num')::integer >= 160000 THEN
EXECUTE 'GRANT %s TO borq_openbao WITH ADMIN TRUE, INHERIT FALSE, SET FALSE';
END IF;
END $borq$;
`, verifier, username)
}

func scramVerifier(password string) (string, error) {
	normalized, err := precis.OpaqueString.String(password)
	if err != nil {
		normalized = password
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2.Key([]byte(normalized), salt, 4096, sha256.Size, sha256.New)
	clientMac := hmac.New(sha256.New, key)
	_, _ = clientMac.Write([]byte("Client Key"))
	storedKey := sha256.Sum256(clientMac.Sum(nil))
	serverMac := hmac.New(sha256.New, key)
	_, _ = serverMac.Write([]byte("Server Key"))
	return "SCRAM-SHA-256$4096:" + base64.StdEncoding.EncodeToString(salt) + "$" + base64.StdEncoding.EncodeToString(storedKey[:]) + ":" + base64.StdEncoding.EncodeToString(serverMac.Sum(nil)), nil
}

// ReadServerCa returns only the public server CA after checking the live Cluster ownership.
func (client Client) ReadServerCa(ctx context.Context, input Input) ([]byte, error) {
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cluster.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return nil, errors.New("PostgreSQL cluster ownership conflict")
	}
	name, _, err := unstructured.NestedString(cluster.Object, "spec", "certificates", "serverCASecret")
	if err != nil {
		return nil, errors.New("PostgreSQL certificate configuration is invalid")
	}
	if name == "" {
		name = input.Name + "-ca"
	}
	secret, err := client.kube.CoreV1().Secrets(input.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.New("PostgreSQL server CA is unavailable")
	}
	owned := false
	for _, owner := range secret.OwnerReferences {
		if owner.Kind == "Cluster" && owner.APIVersion == "postgresql.cnpg.io/v1" && owner.Name == input.Name && owner.UID == cluster.GetUID() {
			owned = true
		}
	}
	if !owned {
		return nil, errors.New("PostgreSQL server CA ownership conflict")
	}
	certificate := secret.Data["ca.crt"]
	block, _ := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("PostgreSQL server CA certificate is invalid")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !parsed.IsCA {
		return nil, errors.New("PostgreSQL server CA certificate is invalid")
	}
	return append([]byte{}, certificate...), nil
}

// SetServerDnsName adds the private management endpoint to CNPG's generated server certificate.
func (client Client) SetServerDnsName(ctx context.Context, input Input, hostname string) error {
	if hostname == "" || strings.ContainsAny(hostname, "/\\:@?# \t\r\n") {
		return errors.New("PostgreSQL server DNS name is invalid")
	}
	handle := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace)
	cluster, err := handle.Get(ctx, input.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cluster.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return errors.New("PostgreSQL cluster ownership conflict")
	}
	current, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "certificates", "serverAltDNSNames")
	for _, name := range current {
		if name == hostname {
			return nil
		}
	}
	current = append(current, hostname)
	if err := unstructured.SetNestedStringSlice(cluster.Object, current, "spec", "certificates", "serverAltDNSNames"); err != nil {
		return err
	}
	_, err = handle.Update(ctx, cluster, metav1.UpdateOptions{})
	return err
}
