//go:build integration

package dbtest

import (
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestPostgresWorkload owns real Postgres fixture persistence, locking and loss detection.
// Each case uses an isolated schema in the explicitly supplied disposable database.
func TestPostgresWorkload(t *testing.T) {
	address := os.Getenv("BORQ_ADDON_TEST_POSTGRES_URL")
	if address == "" {
		t.Skip("BORQ_ADDON_TEST_POSTGRES_URL is required")
	}
	runWorkloadSuite(t, "postgres", func(t *testing.T) string {
		assert := is.New(t)
		database := openTestDatabase(t, "postgres", address)
		name := fmt.Sprintf("probe_%d", time.Now().UnixNano())
		_, err := database.Exec("CREATE SCHEMA " + name)
		assert.NoErr(err)
		t.Cleanup(func() {
			_, err := database.Exec("DROP SCHEMA " + name + " CASCADE")
			assert.NoErr(err)
		})
		connection, err := url.Parse(address)
		assert.NoErr(err)
		parameters := connection.Query()
		parameters.Set("search_path", name)
		connection.RawQuery = parameters.Encode()
		return connection.String()
	})
}
