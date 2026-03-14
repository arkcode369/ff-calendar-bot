package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
)

// EventRepo implements ports.EventRepository using BadgerDB.
type EventRepo struct {
	db *badger.DB
}

// NewEventRepo creates a new EventRepo backed by the given DB.
func NewEventRepo(db *DB) *EventRepo {
	return &EventRepo{db: db.Badger()}
}

// --- Key builders ---

func eventKey(date time.Time, eventID string) []byte {
	return []byte(fmt.Sprintf("evt:%s:%s", date.Format("20060102"), eventID))
}

func eventPrefix(date time.Time) []byte {
	return []byte(fmt.Sprintf("evt:%s:", date.Format("20060102")))
}

func eventHistKey(currency, eventName string, date time.Time) []byte {
	// Normalize event name: lowercase, replace spaces with dashes
	name := strings.ToLower(strings.ReplaceAll(eventName, " ", "-"))
	return []byte(fmt.Sprintf("evthist:%s:%s:%s", currency, name, date.Format("20060102")))
}

func eventHistPrefix(currency, eventName string) []byte {
	name := strings.ToLower(strings.ReplaceAll(eventName, " ", "-"))
	return []byte(fmt.Sprintf("evthist:%s:%s:", currency, name))
}

func revisionKey(currency string, date time.Time, eventID string) []byte {
	return []byte(fmt.Sprintf("evtrev:%s:%s:%s", currency, date.Format("20060102"), eventID))
}

func revisionPrefix(currency string) []byte {
	return []byte(fmt.Sprintf("evtrev:%s:", currency))
}

// --- EventRepository interface implementation ---

// SaveEvents stores a batch of FFEvent records.
// Uses WriteBatch for efficient bulk inserts.
func (r *EventRepo) SaveEvents(_ context.Context, events []domain.FFEvent) error {
	wb := r.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range events {
		data, err := json.Marshal(&events[i])
		if err != nil {
			return fmt.Errorf("marshal event %s: %w", events[i].ID, err)
		}
		key := eventKey(events[i].Date, events[i].ID)
		if err := wb.Set(key, data); err != nil {
			return fmt.Errorf("batch set event %s: %w", events[i].ID, err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("flush events batch: %w", err)
	}
	return nil
}

// GetEventsByDateRange returns all events within [start, end] inclusive.
// Iterates day-by-day using prefix scans for each date.
func (r *EventRepo) GetEventsByDateRange(_ context.Context, start, end time.Time) ([]domain.FFEvent, error) {
	var events []domain.FFEvent

	err := r.db.View(func(txn *badger.Txn) error {
		// Iterate each day in range
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			prefix := eventPrefix(d)
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			opts.PrefetchValues = true
			opts.PrefetchSize = 50

			it := txn.NewIterator(opts)
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				item := it.Item()
				err := item.Value(func(val []byte) error {
					var evt domain.FFEvent
					if err := json.Unmarshal(val, &evt); err != nil {
						return err
					}
					events = append(events, evt)
					return nil
				})
				if err != nil {
					it.Close()
					return fmt.Errorf("read event at %s: %w", item.Key(), err)
				}
			}
			it.Close()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get events by date range: %w", err)
	}
	return events, nil
}

// GetEventHistory returns historical data points for a specific event.
// Scans evthist:{currency}:{eventName}: prefix in reverse chronological order.
func (r *EventRepo) GetEventHistory(_ context.Context, eventName, currency string, months int) ([]domain.FFEventDetail, error) {
	var history []domain.FFEventDetail

	cutoff := time.Now().AddDate(0, -months, 0).Format("20060102")
	prefix := eventHistPrefix(currency, eventName)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true
		opts.PrefetchSize = 30

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			// Extract date from key: evthist:{curr}:{name}:{YYYYMMDD}
			parts := strings.Split(string(item.Key()), ":")
			if len(parts) >= 4 && parts[3] < cutoff {
				continue // skip entries older than cutoff
			}

			err := item.Value(func(val []byte) error {
				var detail domain.FFEventDetail
				if err := json.Unmarshal(val, &detail); err != nil {
					return err
				}
				history = append(history, detail)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read event history: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get event history %s/%s: %w", currency, eventName, err)
	}
	return history, nil
}

// SaveEventDetails stores historical data points for an event.
// Uses EventName and Currency from each detail to build the storage key.
func (r *EventRepo) SaveEventDetails(_ context.Context, details []domain.FFEventDetail) error {
	wb := r.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range details {
		data, err := json.Marshal(&details[i])
		if err != nil {
			return fmt.Errorf("marshal event detail: %w", err)
		}
		key := eventHistKey(details[i].Currency, details[i].EventName, details[i].Date)
		if err := wb.Set(key, data); err != nil {
			return fmt.Errorf("batch set event detail: %w", err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("flush event details batch: %w", err)
	}
	return nil
}

// GetEventsByDate retrieves all events for a specific date.
func (r *EventRepo) GetEventsByDate(ctx context.Context, date time.Time) ([]domain.FFEvent, error) {
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	end := start.AddDate(0, 0, 1)
	return r.GetEventsByDateRange(ctx, start, end)
}

// GetHighImpactEvents retrieves only high-impact events in date range.
func (r *EventRepo) GetHighImpactEvents(ctx context.Context, start, end time.Time) ([]domain.FFEvent, error) {
	all, err := r.GetEventsByDateRange(ctx, start, end)
	if err != nil {
		return nil, err
	}
	var result []domain.FFEvent
	for _, ev := range all {
		if ev.Impact == domain.ImpactHigh {
			result = append(result, ev)
		}
	}
	return result, nil
}

// GetEventsByCurrency retrieves events filtered by currency code.
func (r *EventRepo) GetEventsByCurrency(ctx context.Context, currency string, start, end time.Time) ([]domain.FFEvent, error) {
	all, err := r.GetEventsByDateRange(ctx, start, end)
	if err != nil {
		return nil, err
	}
	var result []domain.FFEvent
	for _, ev := range all {
		if ev.Currency == currency {
			result = append(result, ev)
		}
	}
	return result, nil
}

// GetAllRevisions retrieves all revisions within the last N days.
func (r *EventRepo) GetAllRevisions(_ context.Context, days int) ([]domain.EventRevision, error) {
	var revisions []domain.EventRevision
	cutoff := time.Now().AddDate(0, 0, -days).Format("20060102")
	prefix := []byte("evtrev:")

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			parts := strings.Split(string(item.Key()), ":")
			if len(parts) >= 3 && parts[2] < cutoff {
				continue
			}
			err := item.Value(func(val []byte) error {
				var rev domain.EventRevision
				if err := json.Unmarshal(val, &rev); err != nil {
					return err
				}
				revisions = append(revisions, rev)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read revision: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all revisions: %w", err)
	}
	return revisions, nil
}

// SaveRevision stores an event revision record.
func (r *EventRepo) SaveRevision(_ context.Context, rev domain.EventRevision) error {
	data, err := json.Marshal(&rev)
	if err != nil {
		return fmt.Errorf("marshal revision: %w", err)
	}

	key := revisionKey(rev.Currency, rev.RevisionDate, rev.EventID)
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save revision: %w", err)
	}
	return nil
}

// GetRevisions returns all revisions for a currency within the last N days.
func (r *EventRepo) GetRevisions(_ context.Context, currency string, days int) ([]domain.EventRevision, error) {
	var revisions []domain.EventRevision

	cutoff := time.Now().AddDate(0, 0, -days).Format("20060102")
	prefix := revisionPrefix(currency)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			// Key: evtrev:{currency}:{YYYYMMDD}:{eventID}
			parts := strings.Split(string(item.Key()), ":")
			if len(parts) >= 3 && parts[2] < cutoff {
				continue
			}

			err := item.Value(func(val []byte) error {
				var rev domain.EventRevision
				if err := json.Unmarshal(val, &rev); err != nil {
					return err
				}
				revisions = append(revisions, rev)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read revision: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get revisions %s: %w", currency, err)
	}
	return revisions, nil
}

// GetEvent retrieves a single event by date and ID.
func (r *EventRepo) GetEvent(_ context.Context, date time.Time, eventID string) (*domain.FFEvent, error) {
	var evt domain.FFEvent

	key := eventKey(date, eventID)
	err := r.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &evt)
		})
	})
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get event %s: %w", eventID, err)
	}
	return &evt, nil
}

// DeleteEventsByDate removes all events for a specific date.
// Used for refresh operations where we re-scrape an entire day.
func (r *EventRepo) DeleteEventsByDate(_ context.Context, date time.Time) error {
	prefix := eventPrefix(date)

	return r.db.Update(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false // keys only

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().KeyCopy(nil)
			if err := txn.Delete(key); err != nil {
				return fmt.Errorf("delete event key %s: %w", key, err)
			}
		}
		return nil
	})
}

// CountEvents returns the number of events stored for a date range.
func (r *EventRepo) CountEvents(_ context.Context, start, end time.Time) (int, error) {
	count := 0

	err := r.db.View(func(txn *badger.Txn) error {
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			prefix := eventPrefix(d)
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			opts.PrefetchValues = false

			it := txn.NewIterator(opts)
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				count++
			}
			it.Close()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return count, nil
}
