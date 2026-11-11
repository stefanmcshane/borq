package offsite

import (
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestSpaceGenerationSelection uses captured chronology, fails closed on ambiguity,
// and preserves PostgreSQL objects in the next live source after space recovery.
func TestSpaceGenerationSelection(t *testing.T) {
	space := strings.Repeat("a", 32)
	for _, application := range []string{"web", "pocketid/" + strings.Repeat("d", 32)} {
		t.Run(application, func(t *testing.T) {
			root := "sqlite/" + space + "/" + application + "/generations/"
			first, second, third := strings.Repeat("1", 16), strings.Repeat("2", 16), strings.Repeat("3", 16)
			old := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			for _, test := range []struct {
				name      string
				times     []time.Time
				selection string
				want      string
				errorText string
			}{
				{name: "newest", times: []time.Time{old, old.Add(time.Hour)}, want: second},
				{name: "early-tie-with-newer", times: []time.Time{old, old, old.Add(time.Hour)}, want: third},
				{name: "tied-newest", times: []time.Time{old, old}, errorText: "ambiguous"},
				{name: "missing-earlier-chronology", times: []time.Time{{}, old}, errorText: "ambiguous"},
				{name: "missing-later-chronology", times: []time.Time{old, {}}, errorText: "ambiguous"},
				{name: "explicit-older", times: []time.Time{old, old.Add(time.Hour)}, selection: first, want: first},
				{name: "explicit-tie", times: []time.Time{old, old}, selection: second, want: second},
				{name: "absent-selection", times: []time.Time{old}, selection: second, errorText: "absent"},
			} {
				t.Run(test.name, func(t *testing.T) {
					assert := is.New(t)
					archive := Archive{Version: 3, Objects: []Object{{Key: archiveServer() + "/base/20261024T120000/data.tar.gz", Size: 3, SHA256: strings.Repeat("b", 64)}}}
					for index, modified := range test.times {
						generation := root + []string{first, second, third}[index]
						archive.Sqlite = append(archive.Sqlite, generation)
						archive.Objects = append(archive.Objects, Object{Key: generation + "/snapshots/00000000.snapshot.lz4", LastModified: modified, Size: 8, SHA256: strings.Repeat("c", 64)})
					}
					selected := map[string]string{}
					if test.selection != "" {
						selected[application] = test.selection
					}
					objects, err := SelectSpaceObjects(archive, space, selected)
					if test.errorText != "" {
						assert.True(err != nil && strings.Contains(err.Error(), test.errorText))
						return
					}
					assert.NoErr(err)
					assert.Equal(len(objects), 2)
					assert.Equal(objects[0].Key, archive.Objects[0].Key)
					assert.Equal(objects[1].Key, root+test.want+"/snapshots/00000000.snapshot.lz4")
				})
			}
		})
	}
}
