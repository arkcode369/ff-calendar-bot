package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
)

// SurpriseRepo implements ports.SurpriseRepository using BadgerDB.
type SurpriseRepo struct {
	db *badger.DB
}

// NewSurpriseRepo creates a new SurpriseRepo backed by the given DB.
func NewSurpriseRepo(db *DB) *SurpriseRepo {
	return &SurpriseRepo{db: db.Badger()}
}

// --- Key builders ---

func surpriseKey(currency string, date time.Time, eventID string) []byte {
	return []byte(fmt.Sprintf("surp:%s:%s:%s", currency, date.Format("20060102"), eventID))
}

func surprisePrefix(currency string) []byte {
	return []byte(fmt.Sprintf("surp:%s:", currency))
}

func surpriseIndexKey(currency string, date time.Time) []byte {
	return []byte(fmt.Sprintf("surpidx:%s:%s", currency, date.Format("20060102")))
}

func surpriseIndexPrefix(currency string) []byte {
	return []byte(fmt.Sprintf("surpidx:%s:", currency))
}

func confluenceKey(pair string, date time.Time) []byte {
	return []byte(fmt.Sprintf("confl:%s:%s", pair, date.Format("20060102")))
}

func confluencePrefix(pair string) []byte {
	return []byte(fmt.Sprintf("confl:%s:", pair))
}

// --- SurpriseRepository interface implementation ---

// SaveSurprise stores a single surprise score.
func (r *SurpriseRepo) SaveSurprise(_ context.Context, score domain.SurpriseScore) error {
	data, err := json.Marshal(&score)
	if err != nil {
		return fmt.Errorf("marshal surprise score: %w", err)
	}

	key := surpriseKey(score.Currency, score.Timestamp, score.EventID)
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save surprise score: %w", err)
	}
	return nil
}

// SaveSurprises stores a batch of surprise scores.
func (r *SurpriseRepo) SaveSurprises(_ context.Context, scores []domain.SurpriseScore) error {
	wb := r.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range scores {
		data, err := json.Marshal(&scores[i])
		if err != nil {
			return fmt.Errorf("marshal surprise score: %w", err)
		}
		key := surpriseKey(scores[i].Currency, scores[i].Timestamp, scores[i].EventID)
		if err := wb.Set(key, data); err != nil {
			return fmt.Errorf("batch set surprise: %w", err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("flush surprise batch: %w", err)
	}
	return nil
}

// GetSurpriseScores returns surprise scores for a currency within N days.
func (r *SurpriseRepo) GetSurpriseScores(_ context.Context, currency string, days int) ([]domain.SurpriseScore, error) {
	var scores []domain.SurpriseScore

	cutoff := time.Now().AddDate(0, 0, -days).Format("20060102")
	prefix := surprisePrefix(currency)
	seekKey := []byte(fmt.Sprintf("surp:%s:%s", currency, cutoff))

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true
		opts.PrefetchSize = 50

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(seekKey); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var s domain.SurpriseScore
				if err := json.Unmarshal(val, &s); err != nil {
					return err
				}
				scores = append(scores, s)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read surprise score: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get surprise scores %s: %w", currency, err)
	}
	return scores, nil
}

// SaveSurpriseIndex stores a computed surprise index snapshot.
func (r *SurpriseRepo) SaveSurpriseIndex(_ context.Context, idx domain.SurpriseIndex) error {
	data, err := json.Marshal(&idx)
	if err != nil {
		return fmt.Errorf("marshal surprise index: %w", err)
	}

	key := surpriseIndexKey(idx.Currency, idx.Timestamp)
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save surprise index: %w", err)
	}
	return nil
}

// GetSurpriseIndex returns the most recent surprise index for a currency.
func (r *SurpriseRepo) GetSurpriseIndex(_ context.Context, currency string) (*domain.SurpriseIndex, error) {
	var idx *domain.SurpriseIndex

	prefix := surpriseIndexPrefix(currency)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.Reverse = true
		opts.PrefetchValues = true
		opts.PrefetchSize = 1

		it := txn.NewIterator(opts)
		defer it.Close()

		seekKey := append(prefix, 0xFF)
		it.Seek(seekKey)

		if it.ValidForPrefix(prefix) {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				idx = &domain.SurpriseIndex{}
				return json.Unmarshal(val, idx)
			})
			if err != nil {
				return fmt.Errorf("read surprise index: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get surprise index %s: %w", currency, err)
	}
	return idx, nil
}

// GetAllSurpriseIndices retrieves latest indices for all currencies.
func (r *SurpriseRepo) GetAllSurpriseIndices(_ context.Context) ([]domain.SurpriseIndex, error) {
	latestMap := make(map[string]*domain.SurpriseIndex)
	idxPrefix := []byte("surpidx:")

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = idxPrefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(idxPrefix); it.ValidForPrefix(idxPrefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var idx domain.SurpriseIndex
				if err := json.Unmarshal(val, &idx); err != nil {
					return err
				}
				// Keys are sorted, so last per currency wins (latest date)
				latestMap[idx.Currency] = &idx
				return nil
			})
			if err != nil {
				return fmt.Errorf("read surprise index: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all surprise indices: %w", err)
	}

	var result []domain.SurpriseIndex
	for _, idx := range latestMap {
		result = append(result, *idx)
	}
	return result, nil
}

// SaveConfluence stores a confluence score snapshot.
func (r *SurpriseRepo) SaveConfluence(_ context.Context, score domain.ConfluenceScore) error {
	data, err := json.Marshal(&score)
	if err != nil {
		return fmt.Errorf("marshal confluence score: %w", err)
	}

	key := confluenceKey(score.CurrencyPair, score.Timestamp)
	err = r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		return fmt.Errorf("save confluence score: %w", err)
	}
	return nil
}

// GetLatestConfluence returns the most recent confluence score for a pair.
func (r *SurpriseRepo) GetLatestConfluence(_ context.Context, pair string) (*domain.ConfluenceScore, error) {
	var score *domain.ConfluenceScore

	prefix := confluencePrefix(pair)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.Reverse = true
		opts.PrefetchValues = true
		opts.PrefetchSize = 1

		it := txn.NewIterator(opts)
		defer it.Close()

		seekKey := append(prefix, 0xFF)
		it.Seek(seekKey)

		if it.ValidForPrefix(prefix) {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				score = &domain.ConfluenceScore{}
				return json.Unmarshal(val, score)
			})
			if err != nil {
				return fmt.Errorf("read confluence score: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get latest confluence %s: %w", pair, err)
	}
	return score, nil
}

// GetConfluenceHistory returns confluence scores for a pair over N days.
func (r *SurpriseRepo) GetConfluenceHistory(_ context.Context, pair string, days int) ([]domain.ConfluenceScore, error) {
	var scores []domain.ConfluenceScore

	cutoff := time.Now().AddDate(0, 0, -days).Format("20060102")
	prefix := confluencePrefix(pair)
	seekKey := []byte(fmt.Sprintf("confl:%s:%s", pair, cutoff))

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(seekKey); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var s domain.ConfluenceScore
				if err := json.Unmarshal(val, &s); err != nil {
					return err
				}
				scores = append(scores, s)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read confluence history: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get confluence history %s: %w", pair, err)
	}
	return scores, nil
}

// GetAllConfluences returns the latest confluence score for every pair.
func (r *SurpriseRepo) GetAllConfluences(_ context.Context) ([]domain.ConfluenceScore, error) {
	latestMap := make(map[string]*domain.ConfluenceScore)
	confPrefix := []byte("confl:")

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = confPrefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(confPrefix); it.ValidForPrefix(confPrefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var s domain.ConfluenceScore
				if err := json.Unmarshal(val, &s); err != nil {
					return err
				}
				// Keys are sorted, so last per pair wins (latest date)
				latestMap[s.CurrencyPair] = &s
				return nil
			})
			if err != nil {
				return fmt.Errorf("read confluence: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all confluences: %w", err)
	}

	var result []domain.ConfluenceScore
	for _, score := range latestMap {
		result = append(result, *score)
	}
	return result, nil
}
