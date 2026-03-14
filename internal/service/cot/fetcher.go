package cot

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
)

// Fetcher retrieves COT data from CFTC Socrata API with CSV fallback.
// Primary: CFTC Socrata Open Data API (JSON)
// Fallback: CFTC bulk CSV download from cftc.gov
type Fetcher struct {
	httpClient *http.Client
	socrataURL string // e.g., "https://publicreporting.cftc.gov/resource/6dca-aqww.json"
	csvURL     string // e.g., "https://www.cftc.gov/dea/newcot/deafut.txt"
}

// NewFetcher creates a COT fetcher with default CFTC endpoints.
func NewFetcher() *Fetcher {
	return &Fetcher{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		socrataURL: "https://publicreporting.cftc.gov/resource/6dca-aqww.json",
		csvURL:     "https://www.cftc.gov/dea/newcot/deafut.txt",
	}
}

// FetchLatest retrieves the most recent COT records for all tracked contracts.
// It tries the Socrata API first, falling back to CSV if that fails.
func (f *Fetcher) FetchLatest(ctx context.Context, contracts []domain.COTContract) ([]domain.COTRecord, error) {
	records, err := f.fetchFromSocrata(ctx, contracts)
	if err != nil {
		log.Printf("[cot] Socrata API failed: %v, falling back to CSV", err)
		records, err = f.fetchFromCSV(ctx, contracts)
		if err != nil {
			return nil, fmt.Errorf("both Socrata and CSV failed: %w", err)
		}
	}

	log.Printf("[cot] fetched %d records for %d contracts", len(records), len(contracts))
	return records, nil
}

// FetchHistory retrieves historical COT data for a specific contract.
// Uses Socrata with $where and $order for efficient server-side filtering.
func (f *Fetcher) FetchHistory(ctx context.Context, contract domain.COTContract, weeks int) ([]domain.COTRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.socrataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	q := req.URL.Query()
	q.Add("$where", fmt.Sprintf("cftc_contract_market_code='%s'", contract.Code))
	q.Add("$order", "report_date_as_yyyy_mm_dd DESC")
	q.Add("$limit", fmt.Sprintf("%d", weeks))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("socrata history: status %d", resp.StatusCode)
	}

	var raw []domain.SocrataRecord
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}

	records := make([]domain.COTRecord, 0, len(raw))
	for _, sr := range raw {
		records = append(records, socrataToRecord(sr, contract))
	}

	return records, nil
}

// fetchFromSocrata queries the CFTC Socrata API for latest data.
func (f *Fetcher) fetchFromSocrata(ctx context.Context, contracts []domain.COTContract) ([]domain.COTRecord, error) {
	// Build $where clause for all tracked contracts
	codes := make([]string, len(contracts))
	for i, c := range contracts {
		codes[i] = fmt.Sprintf("'%s'", c.Code)
	}
	where := fmt.Sprintf("cftc_contract_market_code in(%s)", strings.Join(codes, ","))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.socrataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	q := req.URL.Query()
	q.Add("$where", where)
	q.Add("$order", "report_date_as_yyyy_mm_dd DESC")
	q.Add("$limit", fmt.Sprintf("%d", len(contracts)*2))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("socrata request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("socrata status %d: %s", resp.StatusCode, string(body))
	}

	var raw []domain.SocrataRecord
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode socrata: %w", err)
	}

	// Convert and deduplicate: keep only latest per contract
	contractMap := buildContractMap(contracts)
	seen := make(map[string]bool)
	var records []domain.COTRecord

	for _, sr := range raw {
		contract, ok := contractMap[sr.ContractCode]
		if !ok {
			continue
		}
		if seen[sr.ContractCode] {
			continue // already have latest for this contract
		}
		seen[sr.ContractCode] = true
		records = append(records, socrataToRecord(sr, contract))
	}

	return records, nil
}

// fetchFromCSV downloads and parses the CFTC bulk CSV as fallback.
func (f *Fetcher) fetchFromCSV(ctx context.Context, contracts []domain.COTContract) ([]domain.COTRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.csvURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create csv request: %w", err)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("csv request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("csv status %d", resp.StatusCode)
	}

	reader := csv.NewReader(resp.Body)
	reader.LazyQuotes = true

	// Read header row
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("csv header: %w", err)
	}

	colIdx := buildColumnIndex(header)
	contractMap := buildContractMap(contracts)
	var records []domain.COTRecord

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}

		cftcCode := getCSVField(row, colIdx, "CFTC_Contract_Market_Code")
		contract, ok := contractMap[cftcCode]
		if !ok {
			continue
		}

		record := csvRowToRecord(row, colIdx, contract)
		records = append(records, record)
	}

	return records, nil
}

// --- conversion helpers ---

// socrataFloat parses a string field from Socrata JSON to float64.
func socrataFloat(s string) float64 {
	s = strings.ReplaceAll(s, ",", "")
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// socrataToRecord converts a Socrata JSON record to our domain model.
func socrataToRecord(sr domain.SocrataRecord, contract domain.COTContract) domain.COTRecord {
	reportDate, _ := time.Parse("2006-01-02T15:04:05.000", sr.ReportDate)
	if reportDate.IsZero() && len(sr.ReportDate) >= 10 {
		reportDate, _ = time.Parse("2006-01-02", sr.ReportDate[:10])
	}

	return domain.COTRecord{
		ContractCode: contract.Code,
		ContractName: contract.Name,
		ReportDate:   reportDate,

		CommLong:   socrataFloat(sr.CommLong),
		CommShort:  socrataFloat(sr.CommShort),
		SpecLong:   socrataFloat(sr.SpecLong),
		SpecShort:  socrataFloat(sr.SpecShort),
		SmallLong:  socrataFloat(sr.SmallLong),
		SmallShort: socrataFloat(sr.SmallShort),
		OpenInterest: socrataFloat(sr.OpenInterest),

		CommLongChange:  socrataFloat(sr.CommLongChange),
		CommShortChange: socrataFloat(sr.CommShortChange),
		SpecLongChange:  socrataFloat(sr.SpecLongChange),
		SpecShortChange: socrataFloat(sr.SpecShortChange),
		SmallLongChange: socrataFloat(sr.SmallLongChange),
		SmallShortChange: socrataFloat(sr.SmallShortChange),

		Top4Long:  socrataFloat(sr.Top4Long),
		Top4Short: socrataFloat(sr.Top4Short),
		Top8Long:  socrataFloat(sr.Top8Long),
		Top8Short: socrataFloat(sr.Top8Short),
	}
}

// csvRowToRecord converts a CSV row to a COTRecord.
func csvRowToRecord(row []string, colIdx map[string]int, contract domain.COTContract) domain.COTRecord {
	reportDate, _ := time.Parse("2006-01-02", getCSVField(row, colIdx, "As_of_Date_In_Form_YYMMDD"))
	if reportDate.IsZero() {
		// Try alternate format
		reportDate, _ = time.Parse("060102", getCSVField(row, colIdx, "As_of_Date_In_Form_YYMMDD"))
	}

	return domain.COTRecord{
		ContractCode: contract.Code,
		ContractName: contract.Name,
		ReportDate:   reportDate,

		CommLong:     csvFloat(row, colIdx, "Comm_Positions_Long_All"),
		CommShort:    csvFloat(row, colIdx, "Comm_Positions_Short_All"),
		SpecLong:     csvFloat(row, colIdx, "NonComm_Positions_Long_All"),
		SpecShort:    csvFloat(row, colIdx, "NonComm_Positions_Short_All"),
		SmallLong:    csvFloat(row, colIdx, "NonRept_Positions_Long_All"),
		SmallShort:   csvFloat(row, colIdx, "NonRept_Positions_Short_All"),
		OpenInterest: csvFloat(row, colIdx, "Open_Interest_All"),

		CommLongChange:  csvFloat(row, colIdx, "Change_in_Comm_Long_All"),
		CommShortChange: csvFloat(row, colIdx, "Change_in_Comm_Short_All"),
		SpecLongChange:  csvFloat(row, colIdx, "Change_in_NonComm_Long_All"),
		SpecShortChange: csvFloat(row, colIdx, "Change_in_NonComm_Short_All"),
		SmallLongChange: csvFloat(row, colIdx, "Change_in_NonRept_Long_All"),
		SmallShortChange: csvFloat(row, colIdx, "Change_in_NonRept_Short_All"),

		Top4Long:  csvFloat(row, colIdx, "Pct_of_OI_4_or_Less_Long_All"),
		Top4Short: csvFloat(row, colIdx, "Pct_of_OI_4_or_Less_Short_All"),
		Top8Long:  csvFloat(row, colIdx, "Pct_of_OI_8_or_Less_Long_All"),
		Top8Short: csvFloat(row, colIdx, "Pct_of_OI_8_or_Less_Short_All"),
	}
}

// --- CSV helpers ---

func buildContractMap(contracts []domain.COTContract) map[string]domain.COTContract {
	m := make(map[string]domain.COTContract, len(contracts))
	for _, c := range contracts {
		m[c.Code] = c
	}
	return m
}

func buildColumnIndex(header []string) map[string]int {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	return idx
}

func getCSVField(row []string, colIdx map[string]int, col string) string {
	if i, ok := colIdx[col]; ok && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func csvInt(row []string, colIdx map[string]int, col string) int64 {
	s := getCSVField(row, colIdx, col)
	s = strings.ReplaceAll(s, ",", "")
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func csvFloat(row []string, colIdx map[string]int, col string) float64 {
	s := getCSVField(row, colIdx, col)
	s = strings.ReplaceAll(s, ",", "")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
