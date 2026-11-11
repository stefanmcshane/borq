package pocketid

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// User identifies a normal account whose UUID was persisted before provisioning.
// Provisioning never adopts an account by email or grants provider administration.
type User struct {
	// ID is the durable provider UUID selected by the caller.
	ID string
	// Username is the unique login name selected for this account.
	Username string
	// Email is the invited account's address.
	Email string
}

// EnsureUser creates an ordinary user and verifies the exact identity on retries.
// It never changes an existing user's privileges or contact details.
func (client Client) EnsureUser(ctx context.Context, input User) error {
	if !validUserID(input.ID) || strings.TrimSpace(input.Username) == "" || strings.TrimSpace(input.Email) == "" {
		return errors.New("the Pocket ID user requires a persisted UUID, username, and email")
	}
	endpoint := "/api/users/" + input.ID
	var user userResponse
	err := client.request(ctx, http.MethodGet, endpoint, nil, &user)
	if statusIs(err, http.StatusNotFound) {
		err = client.request(ctx, http.MethodPost, "/api/users", userResponse{ID: input.ID, Username: input.Username, Email: input.Email}, &user)
		if readErr := client.request(ctx, http.MethodGet, endpoint, nil, &user); readErr != nil {
			if err != nil {
				return err
			}
			return readErr
		}
	} else if err != nil {
		return err
	}
	if user.ID != input.ID || user.Username != input.Username || user.Email != input.Email || user.IsAdmin || user.Disabled {
		return errors.New("the existing Pocket ID user does not match this provisioning request")
	}
	return nil
}
