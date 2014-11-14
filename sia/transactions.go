package sia

import (
	"errors"

	"github.com/NebulousLabs/Andromeda/encoding"
	"github.com/NebulousLabs/Andromeda/signatures"
)

// State.addTransactionToPool() adds a transaction to the transaction pool and
// transaction list without verifying it.
func (s *State) addTransactionToPool(t *Transaction) {
	for _, input := range t.Inputs {
		s.TransactionPool[input.OutputID] = t
	}
	s.TransactionList[t.Inputs[0].OutputID] = t
}

// Takes a transaction out of the transaction pool & transaction list.
func (s *State) removeTransactionFromPool(t *Transaction) {
	for _, input := range t.Inputs {
		delete(s.TransactionPool, input.OutputID)
	}
	delete(s.TransactionList, t.Inputs[0].OutputID)
}

// State.reverseTransaction removes a given transaction from the
// ConsensusState, making it as though the transaction had never happened.
func (s *State) reverseTransaction(t Transaction) {
	// Remove all outputs.
	for i := range t.Outputs {
		delete(s.UnspentOutputs, t.OutputID(i))
	}

	// Add all outputs spent by inputs.
	for _, input := range t.Inputs {
		s.UnspentOutputs[input.OutputID] = s.SpentOutputs[input.OutputID]
		delete(s.SpentOutputs, input.OutputID)
	}

	// Delete all outputs created by storage proofs.
	for _, sp := range t.StorageProofs {
		openContract := s.OpenContracts[sp.ContractID]
		outputID, err := openContract.FileContract.StorageProofOutputID(openContract.ContractID, s.Height(), true)
		if err != nil {
			panic(err)
		}
		delete(s.UnspentOutputs, outputID)
	}

	// Delete all the open contracts created by new contracts.
	for i := range t.FileContracts {
		contractID := t.FileContractID(i)
		delete(s.OpenContracts, contractID)
	}
}

// State.applyTransaction() takes a transaction and adds it to the
// ConsensusState, updating the list of contracts, outputs, etc.
func (s *State) applyTransaction(t Transaction) {
	// Remove all inputs from the unspent outputs list.
	for _, input := range t.Inputs {
		s.SpentOutputs[input.OutputID] = s.UnspentOutputs[input.OutputID]
		delete(s.UnspentOutputs, input.OutputID)
	}

	// REMOVE ALL CONFLICTING TRANSACTIONS FROM THE TRANSACTION POOL.

	// Add all outputs to the unspent outputs list
	for i, output := range t.Outputs {
		s.UnspentOutputs[t.OutputID(i)] = output
	}

	// Add all new contracts to the OpenContracts list.
	for i, contract := range t.FileContracts {
		contractID := t.FileContractID(i)
		openContract := OpenContract{
			FileContract:    contract,
			ContractID:      contractID,
			FundsRemaining:  contract.ContractFund,
			Failures:        0,
			WindowSatisfied: true, // The first window is free, because the start is in the future by mandate.
		}
		s.OpenContracts[contractID] = &openContract
	}

	// Add all outputs created by storage proofs.
	for _, sp := range t.StorageProofs {
		// Check for contract termination.
		openContract := s.OpenContracts[sp.ContractID]
		payout := openContract.FileContract.ValidProofPayout
		if openContract.FundsRemaining < openContract.FileContract.ValidProofPayout {
			payout = openContract.FundsRemaining
		}

		output := Output{
			Value:     payout,
			SpendHash: openContract.FileContract.ValidProofAddress,
		}
		outputID, err := openContract.FileContract.StorageProofOutputID(openContract.ContractID, s.Height(), true)
		if err != nil {
			panic(err)
		}
		s.UnspentOutputs[outputID] = output

		// Mark the proof as complete for this window.
		s.OpenContracts[sp.ContractID].WindowSatisfied = true
		s.OpenContracts[sp.ContractID].FundsRemaining -= payout
	}

	// Check the arbitrary data of the transaction to fill out the host database.
	if len(t.ArbitraryData) > 8 {
		dataIndicator := encoding.DecUint64(t.ArbitraryData[0:8])
		if dataIndicator == 1 {
			var ha HostAnnouncement
			encoding.Unmarshal(t.ArbitraryData[1:], ha)

			// Verify that the spend condiitons match.
			if ha.SpendConditions.CoinAddress() != t.Outputs[ha.FreezeIndex].SpendHash {
				return
			}

			// Add the host to the host database.
			host := Host{
				IPAddress:   string(ha.IPAddress),
				MinSize:     ha.MinFilesize,
				MaxSize:     ha.MaxFilesize,
				Duration:    ha.MaxDuration,
				Frequency:   ha.MaxChallengeFrequency,
				Tolerance:   ha.MinTolerance,
				Price:       ha.Price,
				Burn:        ha.Burn,
				Freeze:      Currency(ha.SpendConditions.TimeLock-s.Height()) * t.Outputs[ha.FreezeIndex].Value,
				CoinAddress: ha.CoinAddress,
			}
			if host.Freeze <= 0 {
				return
			}

			// Add the weight of the host to the total weight of the hosts in
			// the host database.
			s.HostList = append(s.HostList, host)
			s.TotalWeight += host.Weight()
		}
	}
}

// Each input has a list of public keys and a required number of signatures.
// This struct keeps track of which public keys have been used and how many
// more signatures are needed.
type InputSignatures struct {
	RemainingSignatures uint64
	PossibleKeys        []signatures.PublicKey
	UsedKeys            map[uint64]struct{}
}

// State.validTransaction returns err = nil if the transaction is valid, otherwise
// returns an error explaining what wasn't valid.
func (s *State) validTransaction(t *Transaction) (err error) {
	// Iterate through each input, summing the value, checking for
	// correctness, and creating an InputSignatures object.
	inputSum := Currency(0)
	inputSignaturesMap := make(map[OutputID]InputSignatures)
	for _, input := range t.Inputs {
		// Check the input spends an existing and valid output.
		utxo, exists := s.UnspentOutputs[input.OutputID]
		if !exists {
			err = errors.New("transaction spends a nonexisting output")
			return
		}

		// Check that the spend conditions match the hash listed in the output.
		if input.SpendConditions.CoinAddress() != s.UnspentOutputs[input.OutputID].SpendHash {
			err = errors.New("spend conditions do not match hash")
			return
		}

		// Check the timelock on the spend conditions is expired.
		if input.SpendConditions.TimeLock > s.Height() {
			err = errors.New("output spent before timelock expiry.")
			return
		}

		// Create the condition for the input signatures and add it to the input signatures map.
		_, exists = inputSignaturesMap[input.OutputID]
		if exists {
			err = errors.New("output spent twice in same transaction")
			return
		}
		var newInputSignatures InputSignatures
		newInputSignatures.RemainingSignatures = input.SpendConditions.NumSignatures
		newInputSignatures.PossibleKeys = input.SpendConditions.PublicKeys
		inputSignaturesMap[input.OutputID] = newInputSignatures

		// Add the input to the coin sum.
		inputSum += utxo.Value
	}

	// Tally up the miner fees and output values.
	outputSum := Currency(0)
	for _, minerFee := range t.MinerFees {
		outputSum += minerFee
	}
	for _, output := range t.Outputs {
		outputSum += output.Value
	}

	// Verify the contracts and tally up the expenditures.
	for _, contract := range t.FileContracts {
		if contract.ContractFund < 0 {
			err = errors.New("contract must be funded.")
			return
		}
		if contract.Start < s.Height() {
			err = errors.New("contract must start in the future.")
			return
		}
		if contract.End <= contract.Start {
			err = errors.New("contract duration must be at least one block.")
			return
		}

		outputSum += contract.ContractFund
	}

	for _, proof := range t.StorageProofs {
		// Check that the proof has not already been submitted.
		if s.OpenContracts[proof.ContractID].WindowSatisfied {
			err = errors.New("storage proof has already been completed for this contract")
			return
		}

		// Check that the proof passes.
	}

	if inputSum != outputSum {
		err = errors.New("inputs do not equal outputs for transaction.")
		return
	}

	for i, sig := range t.Signatures {
		// Check that each signature signs a unique pubkey where
		// RemainingSignatures > 0.
		if inputSignaturesMap[sig.InputID].RemainingSignatures == 0 {
			err = errors.New("friviolous signature detected.")
			return
		}
		_, exists := inputSignaturesMap[sig.InputID].UsedKeys[sig.PublicKeyIndex]
		if exists {
			err = errors.New("public key used twice while signing")
			return
		}

		// Check the timelock on the signature.
		if sig.TimeLock > s.Height() {
			err = errors.New("signature timelock has not expired")
			return
		}

		// Check that the signature matches the public key.
		sigHash := t.SigHash(i)
		if !signatures.VerifyBytes(sigHash[:], inputSignaturesMap[sig.InputID].PossibleKeys[sig.PublicKeyIndex], sig.Signature) {
			err = errors.New("invalid signature in transaction")
			return
		}
	}

	return
}

// State.AcceptTransaction() checks for a conflict of the transaction with the
// transaction pool, then checks that the transaction is valid given the
// current state, then adds the transaction to the transaction pool.
// AcceptTransaction() is thread safe, and can be called concurrently.
func (s *State) AcceptTransaction(t Transaction) (err error) {
	s.Lock()
	defer s.Unlock()

	// Check that the transaction is not in conflict with the transaction
	// pool.
	for _, input := range t.Inputs {
		_, exists := s.TransactionPool[input.OutputID]
		if exists {
			err = errors.New("conflicting transaction exists in transaction pool")
			return
		}
	}

	// Check that the transaction is potentially valid.
	err = s.validTransaction(&t)
	if err != nil {
		return
	}

	// Add the transaction to the pool.
	s.addTransactionToPool(&t)

	// forward transaction to peers
	s.Server.Broadcast(SendVal('T', t))

	return
}
