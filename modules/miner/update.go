package miner

import (
	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

// ReceiveConsensusSetUpdate will update the miner's most recent block. This is
// a part of the ConsensusSetSubscriber interface.
func (m *Miner) ReceiveConsensusSetUpdate(cc modules.ConsensusChange) {
	lockID := m.mu.Lock()
	defer m.mu.Unlock(lockID)

	m.height -= types.BlockHeight(len(cc.RevertedBlocks))
	m.height += types.BlockHeight(len(cc.AppliedBlocks))

	if len(cc.AppliedBlocks) == 0 {
		return
	}

	m.parent = cc.AppliedBlocks[len(cc.AppliedBlocks)-1].ID()
	target, exists1 := m.cs.ChildTarget(m.parent)
	timestamp, exists2 := m.cs.EarliestChildTimestamp(m.parent)
	if build.DEBUG {
		if !exists1 {
			panic("could not get child target")
		}
		if !exists2 {
			panic("could not get child earliest timestamp")
		}
	}
	m.target = target
	m.earliestTimestamp = timestamp
}

// ReceiveUpdatedUnconfirmedTransactions will replace the current unconfirmed
// set of transactions with the input transactions. This is a part of the
// TransactionPoolSubscriber interface.
func (m *Miner) ReceiveUpdatedUnconfirmedTransactions(unconfirmedTransactions []types.Transaction, _ modules.ConsensusChange) {
	lockID := m.mu.Lock()
	defer m.mu.Unlock(lockID)

	m.transactions = nil
	remainingSize := int(types.BlockSizeLimit - 5e3)
	for {
		if len(unconfirmedTransactions) == 0 {
			break
		}
		remainingSize -= len(encoding.Marshal(unconfirmedTransactions[0]))
		if remainingSize < 0 {
			break
		}

		m.transactions = append(m.transactions, unconfirmedTransactions[0])
		unconfirmedTransactions = unconfirmedTransactions[1:]
	}
}
