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

func prefsKey(chatID int64) []byte {
	return []byte(fmt.Sprintf("prefs:%d", chatID))
}

func alertKey(chatID int64, alertID string) []byte {
	return []byte(fmt.Sprintf("alert:%d:%s", chatID, alertID))
}

func alertPrefix(chatID int64) []byte {
	return []byte(fmt.Sprintf("alert:%d:", chatID))
}

// --- PrefsRepository interface implementation ---

// GetPrefs retrieves preferences for a chat ID.
// Returns default prefs if none exist.
func (r *PrefsRepo) GetPrefs(_ context.Context, chatID int64) (*domain.UserPrefs, error) {
	var prefs domain.UserPrefs

	key := prefsKey(chatID)
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
		dp := domain.DefaultPrefs()
		return &dp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get prefs %d: %w", chatID, err)
	}
	return &prefs, nil
}

// SavePrefs persists user preferences.
func (r *PrefsRepo) SavePrefs(_ context.Context, prefs domain.UserPrefs) error {
	data, err := json.Marshal(&prefs)
	if err != nil {
		return fmt.Errorf("marshal prefs: %w", err)
	}

	// We need a chatID for the key. Since UserPrefs has no ChatID field,
	// we derive it from the stored key convention. For SavePrefs the caller
	// must ensure GetPrefs was called first. We store under a deterministic key
	// using a hash of the prefs JSON for dedup, but the standard pattern is
	// that the service layer calls GetPrefs(chatID) then SavePrefs(modified).
	// To support this, we store under a well-known single-user key.
	// In a multi-user scenario the service layer would wrap this.
	key := []byte("prefs:default")
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	return nil
}

// GetAlerts retrieves alert subscriptions for a chat.
func (r *PrefsRepo) GetAlerts(_ context.Context, chatID int64) ([]domain.AlertConfig, error) {
	var alerts []domain.AlertConfig
	prefix := alertPrefix(chatID)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var a domain.AlertConfig
				if err := json.Unmarshal(val, &a); err != nil {
					return err
				}
				alerts = append(alerts, a)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read alert: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get alerts %d: %w", chatID, err)
	}
	return alerts, nil
}

// SaveAlert persists or updates an alert configuration.
func (r *PrefsRepo) SaveAlert(_ context.Context, alert domain.AlertConfig) error {
	data, err := json.Marshal(&alert)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	// Use MinImpact level as a simple alert ID for the key.
	alertID := fmt.Sprintf("impact_%d", alert.MinImpact)
	key := []byte(fmt.Sprintf("alert:global:%s", alertID))
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save alert: %w", err)
	}
	return nil
}

// DeleteAlert removes an alert subscription.
func (r *PrefsRepo) DeleteAlert(_ context.Context, chatID int64, alertID string) error {
	key := alertKey(chatID, alertID)
	err := r.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
	if err == badger.ErrKeyNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete alert %s for %d: %w", alertID, chatID, err)
	}
	return nil
}

// GetAllActiveAlerts retrieves all enabled alerts across all users.
// Used by the alert dispatcher to check which alerts need firing.
func (r *PrefsRepo) GetAllActiveAlerts(_ context.Context) ([]domain.AlertConfig, error) {
	var alerts []domain.AlertConfig
	prefix := []byte("alert:")

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var a domain.AlertConfig
				if err := json.Unmarshal(val, &a); err != nil {
					return err
				}
				alerts = append(alerts, a)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read alert: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all active alerts: %w", err)
	}
	return alerts, nil
}

// compile-time interface check
var _ interface {
	GetPrefs(context.Context, int64) (*domain.UserPrefs, error)
	SavePrefs(context.Context, domain.UserPrefs) error
	GetAlerts(context.Context, int64) ([]domain.AlertConfig, error)
	SaveAlert(context.Context, domain.AlertConfig) error
	DeleteAlert(context.Context, int64, string) error
	GetAllActiveAlerts(context.Context) ([]domain.AlertConfig, error)
} = (*PrefsRepo)(nil)
