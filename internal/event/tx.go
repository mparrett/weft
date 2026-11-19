package event

import (
	"encoding/json"
	"strconv"
	"time"
)

// Batch framing (EVENTS.md §5.5). Every event a batch writes carries FieldTx,
// and the batch ends with one KindTxCommit naming the same tx. Readers apply a
// tx-tagged run only once its commit line is present, so a write torn after the
// first complete line, or still in flight, is not committed.
//
// The marker comes last, rather than a count on every event, because event ids
// are sealed as each event is built and later events name earlier ids (a claim
// on a node added in the same batch). A count is unknown until the batch is
// done, and putting it on each event afterwards would change the ids.
const (
	KindTxCommit = "tx.commit"

	FieldTx   = "tx"    // the batch key, on every event of the batch
	FieldTxOp = "tx_op" // which op of the request produced the event
)

// TxCommit builds the sealed commit marker for a batch of n events.
func TxCommit(tx string, n int, actor string, at time.Time) (Event, error) {
	e := New(KindTxCommit, actor, SourceAsserted, at)
	e[FieldTx] = tx
	e["n"] = intNumber(n)
	return e, e.Seal()
}

// InTx tags an already-built event as part of batch tx, produced by op, and
// reseals it. The constructors seal on return, so the id has to be dropped and
// recomputed: the tags are inside the preimage, which is what makes two
// otherwise identical events in different batches distinct.
func InTx(e Event, tx string, op int) error {
	delete(e, "id")
	e[FieldTx] = tx
	e[FieldTxOp] = intNumber(op)
	return e.Seal()
}

// Int reads an integer field, reporting false when absent or not an integer.
func (e Event) Int(k string) (int, bool) {
	n, ok := e[k].(interface{ Int64() (int64, error) })
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// intNumber is how integer fields are held in memory, matching what Parse
// produces, so a built event and a parsed one read back the same way.
func intNumber(n int) json.Number { return json.Number(strconv.Itoa(n)) }
