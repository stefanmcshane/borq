package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func prepareAdminAuthentication(ctx context.Context, request Request, connection kubernetes.Connection) (Request, error) {
	if err := rejectLegacyBootstrap(ctx, connection, request.Namespace); err != nil {
		return Request{}, err
	}
	if err := checkManagementMode(ctx, connection, request); err != nil {
		return Request{}, err
	}
	if err := connection.Namespaces().Apply(ctx, managementNamespace(request.Namespace)); err != nil {
		return Request{}, fmt.Errorf("apply management namespace: %w", err)
	}
	configuration := connection.RestConfig()
	client, err := coreclient.NewForConfig(&configuration)
	if err != nil {
		return Request{}, fmt.Errorf("create administrator credential client: %w", err)
	}
	secret, err := client.Secrets(request.Namespace).Get(ctx, "borq-admin-auth", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		bytes := make([]byte, 32)
		if _, err := rand.Read(bytes); err != nil {
			return Request{}, fmt.Errorf("generate administrator token: %w", err)
		}
		secret, err = client.Secrets(request.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-admin-auth", Namespace: request.Namespace, Labels: appLabels(borqName, "authentication")}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte(hex.EncodeToString(bytes))}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			secret, err = client.Secrets(request.Namespace).Get(ctx, "borq-admin-auth", metav1.GetOptions{})
		}
	}
	if err != nil {
		return Request{}, fmt.Errorf("read administrator credential: %w", err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return Request{}, fmt.Errorf("administrator Secret has an invalid token; expected at least 32 characters without whitespace")
	}
	request.adminToken = token
	return request, nil
}
