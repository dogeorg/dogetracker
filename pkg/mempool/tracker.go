package mempool

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/dogeorg/doge"
	"github.com/qlpqlp/dogetracker/pkg/spec"
	"github.com/qlpqlp/dogetracker/server/db"
)

type MempoolTracker struct {
	client       spec.Blockchain
	db           *sql.DB
	trackedAddrs map[string]struct {
		id                    int64
		requiredConfirmations int
	}
	stop chan struct{}
}

func NewMempoolTracker(client spec.Blockchain, db *sql.DB, trackedAddrs []string) *MempoolTracker {
	// Initialize the tracker with address IDs and required confirmations
	addrMap := make(map[string]struct {
		id                    int64
		requiredConfirmations int
	})

	// Get address IDs and required confirmations from database
	for _, addr := range trackedAddrs {
		var id int64
		var requiredConfirmations int
		err := db.QueryRow(`
			SELECT id, required_confirmations 
			FROM tracked_addresses 
			WHERE address = $1
		`, addr).Scan(&id, &requiredConfirmations)
		if err != nil {
			log.Printf("Error getting address ID for %s: %v", addr, err)
			continue
		}
		addrMap[addr] = struct {
			id                    int64
			requiredConfirmations int
		}{id, requiredConfirmations}
	}

	return &MempoolTracker{
		client:       client,
		db:           db,
		trackedAddrs: addrMap,
		stop:         make(chan struct{}),
	}
}

func (t *MempoolTracker) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-ticker.C:
			if err := t.checkMempool(); err != nil {
				log.Printf("Error checking mempool: %v", err)
			}
		}
	}
}

func (t *MempoolTracker) Stop() {
	close(t.stop)
}

// RefreshAddresses refreshes the list of tracked addresses from the database
func (t *MempoolTracker) RefreshAddresses() error {
	// Get all tracked addresses from database
	rows, err := t.db.Query(`SELECT address, id, required_confirmations FROM tracked_addresses`)
	if err != nil {
		return fmt.Errorf("failed to get tracked addresses: %v", err)
	}
	defer rows.Close()

	// Create a new map with current addresses
	newAddrMap := make(map[string]struct {
		id                    int64
		requiredConfirmations int
	})

	// Update the map with current addresses
	for rows.Next() {
		var addr string
		var id int64
		var requiredConfirmations int
		if err := rows.Scan(&addr, &id, &requiredConfirmations); err != nil {
			log.Printf("Error scanning tracked address: %v", err)
			continue
		}
		newAddrMap[addr] = struct {
			id                    int64
			requiredConfirmations int
		}{id, requiredConfirmations}
	}

	// Update the tracker's address map
	t.trackedAddrs = newAddrMap
	log.Printf("Refreshed mempool tracker with %d addresses", len(newAddrMap))
	return nil
}

func (t *MempoolTracker) checkMempool() error {
	// First refresh the list of tracked addresses
	if err := t.RefreshAddresses(); err != nil {
		log.Printf("Error refreshing addresses: %v", err)
	}

	// Get all transactions in mempool
	txids, err := t.client.GetMempoolTransactions()
	if err != nil {
		return err
	}

	log.Printf("Found %d transactions in mempool", len(txids))

	// Process each transaction
	for _, txid := range txids {
		// Get raw transaction hex
		txData, err := t.client.GetRawTransaction(txid)
		if err != nil {
			log.Printf("Error getting raw transaction %s: %v", txid, err)
			continue
		}

		// Get transaction timestamp from mempool
		mempoolData, err := t.client.GetMempoolTransaction(txid)
		if err != nil {
			log.Printf("Error getting mempool transaction data %s: %v", txid, err)
			continue
		}

		// Get timestamp from mempool data
		timestamp, ok := mempoolData["time"].(float64)
		if !ok {
			log.Printf("Error getting timestamp from mempool data for %s", txid)
			continue
		}

		// Convert hex to bytes
		txBytes, err := doge.HexDecode(txData["hex"].(string))
		if err != nil {
			log.Printf("Error decoding transaction hex: %v", err)
			continue
		}

		// Decode transaction using doge package
		tx := doge.DecodeTx(txBytes)

		// Process outputs (incoming transactions)
		for _, vout := range tx.VOut {
			// Extract addresses from output script
			scriptType, addr := doge.ClassifyScript(vout.Script, &doge.DogeMainNetChain)
			if scriptType == "" {
				continue
			}

			addrStr := string(addr)
			if addrInfo, exists := t.trackedAddrs[addrStr]; exists {
				// Found a tracked address in the output
				amount := float64(vout.Value) / 1e8 // Convert from satoshis to DOGE

				// Get sender address from inputs
				var senderAddress string
				if len(tx.VIn) > 0 && len(tx.VIn[0].TxID) > 0 {
					// Get the first input's previous transaction
					txIDHex := doge.HexEncodeReversed(tx.VIn[0].TxID)
					prevTxData, err := t.client.GetRawTransaction(txIDHex)
					if err == nil {
						prevTxBytes, err := doge.HexDecode(prevTxData["hex"].(string))
						if err == nil {
							prevTx := doge.DecodeTx(prevTxBytes)
							if int(tx.VIn[0].VOut) < len(prevTx.VOut) {
								prevOut := prevTx.VOut[tx.VIn[0].VOut]
								_, senderAddr := doge.ClassifyScript(prevOut.Script, &doge.DogeMainNetChain)
								senderAddress = string(senderAddr)
							}
						}
					}
				}

				// Check if this transaction already exists
				var existingTxID int64
				err := t.db.QueryRow(`
					SELECT id FROM transactions 
					WHERE address_id = $1 AND tx_id = $2
				`, addrInfo.id, txid).Scan(&existingTxID)

				if err == sql.ErrNoRows {
					// Transaction doesn't exist, create a new one
					transaction := &db.Transaction{
						AddressID:       addrInfo.id,
						TxID:            txid,
						Amount:          amount,
						IsIncoming:      true,
						Status:          "pending",
						Confirmations:   0,
						SenderAddress:   senderAddress,
						ReceiverAddress: addrStr,
						Timestamp:       int64(timestamp),
					}

					// Add transaction to database
					if err := db.AddTransaction(t.db, transaction); err != nil {
						log.Printf("Error adding pending transaction: %v", err)
					} else {
						log.Printf("Added pending transaction for address %s: %s", addrStr, txid)
					}
				}
			}
		}

		// Process inputs (outgoing transactions)
		for _, vin := range tx.VIn {
			// Skip coinbase transactions (they have empty TxID)
			if len(vin.TxID) == 0 {
				continue
			}

			// Get the previous transaction
			txIDHex := doge.HexEncodeReversed(vin.TxID)
			prevTxData, err := t.client.GetRawTransaction(txIDHex)
			if err != nil {
				continue
			}

			prevTxBytes, err := doge.HexDecode(prevTxData["hex"].(string))
			if err != nil {
				continue
			}
			prevTx := doge.DecodeTx(prevTxBytes)

			// Check if the spent output belonged to a tracked address
			if vin.VOut < uint32(len(prevTx.VOut)) {
				prevOut := prevTx.VOut[vin.VOut]
				scriptType, addr := doge.ClassifyScript(prevOut.Script, &doge.DogeMainNetChain)
				if scriptType == "" {
					continue
				}

				addrStr := string(addr)
				if addrInfo, exists := t.trackedAddrs[addrStr]; exists {
					// Found a tracked address in the input
					amount := -float64(prevOut.Value) / 1e8 // Negative for outgoing, convert from satoshis

					// Get receiver address from outputs
					var receiverAddress string
					if len(tx.VOut) > 0 {
						_, receiverAddr := doge.ClassifyScript(tx.VOut[0].Script, &doge.DogeMainNetChain)
						receiverAddress = string(receiverAddr)
					}

					// Check if this transaction already exists
					var existingTxID int64
					err := t.db.QueryRow(`
						SELECT id FROM transactions 
						WHERE address_id = $1 AND tx_id = $2
					`, addrInfo.id, txid).Scan(&existingTxID)

					if err == sql.ErrNoRows {
						// Transaction doesn't exist, create a new one
						transaction := &db.Transaction{
							AddressID:       addrInfo.id,
							TxID:            txid,
							Amount:          amount,
							IsIncoming:      false,
							Status:          "pending",
							Confirmations:   0,
							SenderAddress:   addrStr,
							ReceiverAddress: receiverAddress,
							Timestamp:       int64(timestamp),
						}

						// Add transaction to database
						if err := db.AddTransaction(t.db, transaction); err != nil {
							log.Printf("Error adding pending transaction: %v", err)
						} else {
							log.Printf("Added pending transaction for address %s: %s", addrStr, txid)
						}
					}
				}
			}
		}
	}

	return nil
}

// AddAddress adds a new address to track in the mempool
func (t *MempoolTracker) AddAddress(address string) {
	// Get address ID and required confirmations from database
	var id int64
	var requiredConfirmations int
	err := t.db.QueryRow(`
		SELECT id, required_confirmations 
		FROM tracked_addresses 
		WHERE address = $1
	`, address).Scan(&id, &requiredConfirmations)
	if err != nil {
		log.Printf("Error getting address ID for %s: %v", address, err)
		return
	}

	t.trackedAddrs[address] = struct {
		id                    int64
		requiredConfirmations int
	}{id, requiredConfirmations}
	log.Printf("Added address to mempool tracker: %s", address)
}
