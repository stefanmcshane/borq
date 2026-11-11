package pocketid

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
)

// TestPocketIdNormalUserProvisioning owns retry recovery and the refusal to
// adopt, modify or promote an unrelated provider account.
func TestPocketIdNormalUserProvisioning(t *testing.T) {
	for _, scenario := range []string{"create", "lost create response", "existing", "admin", "disabled", "email mismatch", "id mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			input := User{ID: "b7bd858a-95b7-4a24-bb17-c47c9126e320", Username: "borq-member", Email: "member@example.test"}
			user := userResponse{ID: input.ID, Username: input.Username, Email: input.Email}
			exists := scenario != "create" && scenario != "lost create response"
			switch scenario {
			case "admin":
				user.IsAdmin = true
			case "disabled":
				user.Disabled = true
			case "email mismatch":
				user.Email = "other@example.test"
			case "id mismatch":
				user.ID = "0c7a7d44-57bf-4e0d-8489-b4365de597fb"
			}
			posts := 0
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Header.Get("X-API-KEY"), "test-private-api-key")
				switch request.Method {
				case http.MethodGet:
					assert.Equal(request.URL.Path, "/api/users/"+input.ID)
					if !exists {
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(writer).Encode(user)
				case http.MethodPost:
					assert.Equal(request.URL.Path, "/api/users")
					posts++
					var created userResponse
					assert.NoErr(json.NewDecoder(request.Body).Decode(&created))
					assert.True(!created.IsAdmin)
					assert.Equal(created.ID, input.ID)
					user = created
					exists = true
					if scenario == "lost create response" {
						writer.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(writer).Encode(created)
				default:
					t.Errorf("unexpected mutation %s", request.Method)
				}
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: "test-private-api-key"})
			assert.NoErr(err)
			err = client.EnsureUser(t.Context(), input)
			if scenario == "create" || scenario == "lost create response" || scenario == "existing" {
				assert.NoErr(err)
				assert.NoErr(client.EnsureUser(t.Context(), input))
				if scenario != "existing" {
					assert.Equal(posts, 1)
				}
			} else {
				assert.True(err != nil)
				assert.Equal(posts, 0)
			}
		})
	}
}
