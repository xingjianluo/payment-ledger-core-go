package reconciliation

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

func TestCSVFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "statement.csv")
	content := "txn_id,external_txn_id,amount,currency,occurred_at\n" +
		"pay-0002,EXT-2,19000,CNY,2026-09-30T12:00:00Z\n" +
		"pay-0001,EXT-1,12000,CNY,2026-09-30T11:00:00Z\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := CSVFileSource{Path: path}.Records("batch-1", "CNY")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("期望 2 条记录, 实际 %d", len(recs))
	}
	// 结果应按交易号排序。
	if recs[0].TxnID != "pay-0001" || recs[0].Amount != 12000 {
		t.Fatalf("CSV 解析/排序错误: %+v", recs[0])
	}
	if recs[1].OccurredAt.UTC() != time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("时间解析错误: %v", recs[1].OccurredAt)
	}
	t.Logf("CSV 对账单来源判定依据: 解析 2 条并按交易号排序 => %s(%d), %s(%d)，全程无网络访问",
		recs[0].TxnID, recs[0].Amount, recs[1].TxnID, recs[1].Amount)
}

func TestMemorySourceRecordsSorted(t *testing.T) {
	src := NewMemorySource()
	src.Add("b1",
		domain.StatementRecord{TxnID: "z", Amount: 1, Currency: "CNY"},
		domain.StatementRecord{TxnID: "a", Amount: 2, Currency: "CNY"},
	)
	recs, err := src.Records("b1", "CNY")
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].TxnID != "a" || recs[1].TxnID != "z" {
		t.Fatalf("内存对账单应按交易号排序: %s,%s", recs[0].TxnID, recs[1].TxnID)
	}
}
