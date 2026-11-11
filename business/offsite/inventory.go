package offsite

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// FrozenInventory fixes the source identities before its keys are checkpointed.
type FrozenInventory struct {
	// Source binds the inventory to one bucket and root.
	Source string `json:"source"`
	// CapturedAt is the time the final inventory page was read.
	CapturedAt time.Time `json:"capturedAt"`
	// Objects is the sorted conditional-read inventory; later source additions are excluded.
	Objects []objectstore.Object `json:"objects"`
}

// Freeze records managed object identities without streaming workload data or holding key locks.
func Freeze(ctx context.Context, config objectstore.Config) (FrozenInventory, error) {
	client, err := objectstore.New(config)
	if err != nil {
		return FrozenInventory{}, err
	}
	inventory := FrozenInventory{Source: SourceID(config), Objects: []objectstore.Object{}}
	for _, scope := range []string{"organizations/", "management/", "sqlite/"} {
		continuation := ""
		for {
			page, err := client.ListPage(ctx, scope, continuation, 1000)
			if err != nil {
				return FrozenInventory{}, err
			}
			for _, object := range page.Objects {
				if managedKey(object.Key) || managementbackup.ManagedKey(object.Key) || sqliteKey(object.Key) {
					inventory.Objects = append(inventory.Objects, object)
					if len(inventory.Objects) > 99998 {
						return FrozenInventory{}, errors.New("offsite inventory exceeds 99998 source objects")
					}
				}
			}
			if page.Continuation == "" {
				break
			}
			if page.Continuation == continuation {
				return FrozenInventory{}, errors.New("source pagination did not advance")
			}
			continuation = page.Continuation
		}
	}
	sort.Slice(inventory.Objects, func(i, j int) bool { return inventory.Objects[i].Key < inventory.Objects[j].Key })
	inventory.CapturedAt = time.Now().UTC()
	return inventory, inventory.validate(config)
}

func (inventory FrozenInventory) validate(config objectstore.Config) error {
	if inventory.Source != SourceID(config) || inventory.CapturedAt.IsZero() || len(inventory.Objects) > 99998 {
		return errors.New("offsite frozen inventory does not match this source or supported bounds")
	}
	for index, object := range inventory.Objects {
		if (!managedKey(object.Key) && !managementbackup.ManagedKey(object.Key) && !sqliteKey(object.Key)) || object.ETag == "" || object.Size < 0 || object.Size > objectstore.MaxStreamSize {
			return errors.New("source inventory lacks safe captured object identities")
		}
		if index > 0 && inventory.Objects[index-1].Key >= object.Key {
			return errors.New("source inventory contains duplicate or unsorted objects")
		}
	}
	return nil
}
