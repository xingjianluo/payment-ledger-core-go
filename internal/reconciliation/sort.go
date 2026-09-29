package reconciliation

import (
	"sort"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

// sortDiffs 让差异报告顺序稳定，便于测试与人工核对。
func sortDiffs(d []domain.ReconciliationDiff) {
	sort.SliceStable(d, func(i, j int) bool {
		if d[i].TxnID != d[j].TxnID {
			return d[i].TxnID < d[j].TxnID
		}
		return d[i].Type < d[j].Type
	})
}
