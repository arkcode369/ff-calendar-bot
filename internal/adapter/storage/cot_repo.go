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

// COTRepo implements ports.COTRepository using BadgerDB.
type COTRepo struct {
	db *badger.DB
}

// NewCOTRepo creates a new COTRepo backed by the given DB.
func NewCOTRepo(db *DB) *COTRepo {
	return &COTRepo{db: db.Badger()}
}

// --- Key builders ---

func cotRecordKey(contractCode string, date time.Time) []byte {
	return []byte(fmt.Sprintf("cot:%s:%s", contractCode, date.Format("20060102")))
}

func cotRecordPrefix(contractCode string) []byte {
	return []byte(fmt.Sprintf("cot:%s:", contractCode))
}

func cotAnalysisKey(contractCode string, date time.Time) []byte {
	return []byte(fmt.Sprintf("cotanl:%s:%s", contractCode, date.Format("20060102")))
}

func cotAnalysisPrefix(contractCode string) []byte {
	return []byte(fmt.Sprintf("cotanl:%s:", contractCode))
}

// --- COTRepository interface implementation ---

// SaveRecords stores a batch of COT records.
func (r *COTRepo) SaveRecords(_ context.Context, records []domain.COTRecord) error {
	wb := r.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range records {
		data, err := json.Marshal(&records[i])
		if err != nil {
			return fmt.Errorf("marshal COT record %s: %w", records[i].ContractCode, err)
		}
		key := cotRecordKey(records[i].ContractCode, records[i].ReportDate)
		if err := wb.Set(key, data); err != nil {
			return fmt.Errorf("batch set COT record: %w", err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("flush COT records batch: %w", err)
	}
	return nil
}

// GetLatest returns the most recent COT record for a contract.
// Uses reverse iteration to find the last entry by date.
func (r *COTRepo) GetLatest(_ context.Context, contractCode string) (*domain.COTRecord, error) {
	var record *domain.COTRecord

	prefix := cotRecordPrefix(contractCode)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.Reverse = true
		opts.PrefetchValues = true
		opts.PrefetchSize = 1

		it := txn.NewIterator(opts)
		defer it.Close()

		// Seek to end of prefix range (append 0xFF)
		seekKey := append(prefix, 0xFF)
		it.Seek(seekKey)

		if it.ValidForPrefix(prefix) {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				record = &domain.COTRecord{}
				return json.Unmarshal(val, record)
			})
			if err != nil {
				return fmt.Errorf("read latest COT record: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get latest COT %s: %w", contractCode, err)
	}
	return record, nil
}

// GetHistory returns COT records for a contract over N weeks.
// Returns records in chronological order (oldest first).
func (r *COTRepo) GetHistory(_ context.Context, contractCode string, weeks int) ([]domain.COTRecord, error) {
	var records []domain.COTRecord

	cutoff := time.Now().AddDate(0, 0, -weeks*7).Format("20060102")
	prefix := cotRecordPrefix(contractCode)
	// Seek directly to the cutoff date within the prefix
	seekKey := []byte(fmt.Sprintf("cot:%s:%s", contractCode, cutoff))

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true
		opts.PrefetchSize = 30

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(seekKey); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var rec domain.COTRecord
				if err := json.Unmarshal(val, &rec); err != nil {
					return err
				}
				records = append(records, rec)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read COT history: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get COT history %s: %w", contractCode, err)
	}
	return records, nil
}

// SaveAnalyses stores a batch of COT analysis results.
func (r *COTRepo) SaveAnalyses(_ context.Context, analyses []domain.COTAnalysis) error {
	wb := r.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range analyses {
		data, err := json.Marshal(&analyses[i])
		if err != nil {
			return fmt.Errorf("marshal COT analysis %s: %w", analyses[i].Contract.Code, err)
		}
		key := cotAnalysisKey(analyses[i].Contract.Code, analyses[i].ReportDate)
		if err := wb.Set(key, data); err != nil {
			return fmt.Errorf("batch set COT analysis: %w", err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("flush COT analyses batch: %w", err)
	}
	return nil
}

// GetLatestAnalysis returns the most recent COT analysis for a contract.
func (r *COTRepo) GetLatestAnalysis(_ context.Context, contractCode string) (*domain.COTAnalysis, error) {
	var analysis *domain.COTAnalysis

	prefix := cotAnalysisPrefix(contractCode)

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
				analysis = &domain.COTAnalysis{}
				return json.Unmarshal(val, analysis)
			})
			if err != nil {
				return fmt.Errorf("read latest COT analysis: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get latest COT analysis %s: %w", contractCode, err)
	}
	return analysis, nil
}

// GetAllLatestAnalyses returns the latest analysis for every contract.
// Scans all cotanl: keys and keeps only the most recent per contract.
func (r *COTRepo) GetAllLatestAnalyses(_ context.Context) ([]domain.COTAnalysis, error) {
	latest := make(map[string]*domain.COTAnalysis)

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte("cotanl:")
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek([]byte("cotanl:")); it.ValidForPrefix([]byte("cotanl:")); it.Next() {
			item := it.Item()
			// Key: cotanl:{contractCode}:{YYYYMMDD}
			parts := strings.Split(string(item.Key()), ":")
			if len(parts) < 3 {
				continue
			}
			contractCode := parts[1]

			err := item.Value(func(val []byte) error {
				var a domain.COTAnalysis
				if err := json.Unmarshal(val, &a); err != nil {
					return err
				}
				// Since keys are sorted, last one per contract is the latest
				latest[contractCode] = &a
				return nil
			})
			if err != nil {
				return fmt.Errorf("read COT analysis: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get all latest analyses: %w", err)
	}

	result := make([]domain.COTAnalysis, 0, len(latest))
	for _, a := range latest {
		result = append(result, *a)
	}
	return result, nil
}

// GetAnalysisHistory returns COT analyses for a contract over N weeks.
func (r *COTRepo) GetAnalysisHistory(_ context.Context, contractCode string, weeks int) ([]domain.COTAnalysis, error) {
	var analyses []domain.COTAnalysis

	cutoff := time.Now().AddDate(0, 0, -weeks*7).Format("20060102")
	prefix := cotAnalysisPrefix(contractCode)
	seekKey := []byte(fmt.Sprintf("cotanl:%s:%s", contractCode, cutoff))

	err := r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(seekKey); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				var a domain.COTAnalysis
				if err := json.Unmarshal(val, &a); err != nil {
					return err
				}
				analyses = append(analyses, a)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read COT analysis history: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get COT analysis history %s: %w", contractCode, err)
	}
	return analyses, nil
}
