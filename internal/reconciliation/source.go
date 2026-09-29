package reconciliation

import (
	"encoding/csv"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

// MemorySource 用内存数据构造对账单，主要用于测试与本地演示。
type MemorySource struct {
	ByBatch map[string][]domain.StatementRecord
}

func NewMemorySource() *MemorySource {
	return &MemorySource{ByBatch: map[string][]domain.StatementRecord{}}
}

func (m *MemorySource) Add(batchID string, records ...domain.StatementRecord) {
	m.ByBatch[batchID] = append(m.ByBatch[batchID], records...)
}

func (m *MemorySource) Records(batchID string, currency domain.Currency) ([]domain.StatementRecord, error) {
	recs := m.ByBatch[batchID]
	out := append([]domain.StatementRecord(nil), recs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TxnID < out[j].TxnID })
	return out, nil
}

// CSVFileSource 从本地 CSV 文件读取对账单（无任何网络依赖）。
// 列格式（含表头）：txn_id,external_txn_id,amount,currency,occurred_at_rfc3339
type CSVFileSource struct {
	Path string
}

func (f CSVFileSource) Records(batchID string, currency domain.Currency) ([]domain.StatementRecord, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []domain.StatementRecord
	for i, row := range rows {
		if i == 0 {
			continue // 表头
		}
		if len(row) < 5 {
			continue
		}
		amt, err := strconv.ParseInt(row[2], 10, 64)
		if err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339, row[4])
		if err != nil {
			return nil, err
		}
		out = append(out, domain.StatementRecord{
			TxnID:         row[0],
			ExternalTxnID: row[1],
			Amount:        domain.Money(amt),
			Currency:      domain.Currency(row[3]),
			OccurredAt:    ts,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TxnID < out[j].TxnID })
	return out, nil
}
