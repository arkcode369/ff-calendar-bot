package storage

import (
	"context"
	"encoding/json"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
)

// PrefsRepo implements ports.PrefsRepository using BadgerDB.
type PrefsRepo struct {
	db *badger.DB
}

// NewPrefsRepo creates a new PrefsRepo backed by the given DB.
func NewPrefsRepo(db *DB) *PrefsRepo {
	return &PrefsRepo{db: db.Badger()}
}

// --- Key builders ---

func prefsKey(userID string) []byte {
	return []byte(fmt.Sprintf("prefs:%s", userID))
}

// --- PrefsRepository interface implementation ---

// GetPrefs retrieves user preferences by user ID.
// Returns default preferences if none are stored.
func (r *PrefsRepo) GetPrefs(_ context.Context, userID string) (*domain.UserPrefs, error) {
	var prefs domain.UserPrefs

	key := prefsKey(userID)
	err := r.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &prefs)
		})
	})

	if err == badger.ErrKeyNotFound {
		// Return defaults
		return domain.DefaultPrefs(userID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("get prefs %s: %w", userID, err)
	}
	return &prefs, nil
}

// SavePrefs stores user preferences.
func (r *PrefsRepo) SavePrefs(_ context.Context, prefs *domain.UserPrefs) error {
	data, err := json.Marshal(prefs)
	if err != nil {
		return fmt.Errorf("marshal prefs: %w", err)
	}

	key := prefsKey(prefs.UserID)
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save prefs %s: %w", prefs.UserID, err)
	}
	return nil
}

// DeletePrefs removes user preferences, reverting to defaults.
func (r *PrefsRepo) DeletePrefs(_ context.Context, userID string) error {
	key := prefsKey(userID)
	err := r.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
	if err == badger.ErrKeyNotFound {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("delete prefs %s: %w", userID, err)
	}
	return nil
}

// UpdateAlertMinutes updates just the alert-before-minutes preference.
func (r *PrefsRepo) UpdateAlertMinutes(ctx context.Context, userID string, minutes int) error {
	prefs, err := r.GetPrefs(ctx, userID)
	if err != nil {
		return err
	}
	prefs.AlertMinutesBefore = minutes
	return r.SavePrefs(ctx, prefs)
}

// UpdateImpactFilter updates the minimum impact level filter.
func (r *PrefsRepo) UpdateImpactFilter(ctx context.Context, userID string, minImpact int) error {
	prefs, err := r.GetPrefs(ctx, userID)
	if err != nil {
		return err
	}
	prefs.MinImpactLevel = minImpact
	return r.SavePrefs(ctx, prefs)
}

// ToggleCurrency adds or removes a currency from the watch list.
func (r *PrefsRepo) ToggleCurrency(ctx context.Context, userID, currency string) (bool, error) {
	prefs, err := r.GetPrefs(ctx, userID)
	if err != nil {
		return false, err
	}

	// Check if currency exists in list
	for i, c := range prefs.WatchCurrencies {
		if c == currency {
			// Remove it
			prefs.WatchCurrencies = append(prefs.WatchCurrencies[:i], prefs.WatchCurrencies[i+1:]...)
			return false, r.SavePrefs(ctx, prefs) // false = removed
		}
	}

	// Add it
	prefs.WatchCurrencies = append(prefs.WatchCurrencies, currency)
	return true, r.SavePrefs(ctx, prefs) // true = added
}

// ToggleAlert enables or disables event alerts.
func (r *PrefsRepo) ToggleAlert(ctx context.Context, userID string, enabled bool) error {
	prefs, err := r.GetPrefs(ctx, userID)
	if err != nil {
		return err
	}
	prefs.AlertEnabled = enabled
	return r.SavePrefs(ctx, prefs)
}

// GetAllPrefs returns preferences for all users.
// Used for batch operations like sending alerts to all subscribed users.
func (r *PrefsRepo) GetAllPrefs(_ context.Context) ([]*domain.UserPrefs, error) {
	var allPrefs []*domain.UserPrefs
	prefix := []byte("prefs:")

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var p domain.UserPrefs
				if err := json.Unmarshal(val, &p); err != nil {
					return err
				}
				allPrefs = append(allPrefs, &p)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read user prefs: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all prefs: %w", err)
	}
	return allPrefs, nil
}
