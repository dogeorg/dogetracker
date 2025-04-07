package db

import (
	"database/sql"
)

// DB operations for tracked addresses
func GetOrCreateAddress(db *sql.DB, address string) (*TrackedAddress, error) {
	var addr TrackedAddress

	// Try to get existing address
	err := db.QueryRow(`
		SELECT id, address, balance, created_at, updated_at 
		FROM tracked_addresses 
		WHERE address = $1
	`, address).Scan(&addr.ID, &addr.Address, &addr.Balance, &addr.CreatedAt, &addr.UpdatedAt)

	if err == sql.ErrNoRows {
		// Create new address
		err = db.QueryRow(`
			INSERT INTO tracked_addresses (address, balance) 
			VALUES ($1, 0) 
			RETURNING id, address, balance, created_at, updated_at
		`, address).Scan(&addr.ID, &addr.Address, &addr.Balance, &addr.CreatedAt, &addr.UpdatedAt)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return &addr, nil
}

// Get address balance and details
func GetAddressDetails(db *sql.DB, address string) (*TrackedAddress, []Transaction, []UnspentOutput, error) {
	addr, err := GetOrCreateAddress(db, address)
	if err != nil {
		return nil, nil, nil, err
	}

	// Get transactions
	rows, err := db.Query(`
		SELECT id, address_id, tx_id, block_hash, block_height, amount, is_incoming, confirmations, created_at
		FROM transactions
		WHERE address_id = $1
		ORDER BY created_at DESC
	`, addr.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	var transactions []Transaction
	for rows.Next() {
		var tx Transaction
		err := rows.Scan(&tx.ID, &tx.AddressID, &tx.TxID, &tx.BlockHash, &tx.BlockHeight,
			&tx.Amount, &tx.IsIncoming, &tx.Confirmations, &tx.CreatedAt)
		if err != nil {
			return nil, nil, nil, err
		}
		transactions = append(transactions, tx)
	}

	// Get unspent outputs
	rows, err = db.Query(`
		SELECT id, address_id, tx_id, vout, amount, script, created_at
		FROM unspent_outputs
		WHERE address_id = $1
	`, addr.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	var unspentOutputs []UnspentOutput
	for rows.Next() {
		var output UnspentOutput
		err := rows.Scan(&output.ID, &output.AddressID, &output.TxID, &output.Vout,
			&output.Amount, &output.Script, &output.CreatedAt)
		if err != nil {
			return nil, nil, nil, err
		}
		unspentOutputs = append(unspentOutputs, output)
	}

	return addr, transactions, unspentOutputs, nil
}

// Update address balance
func UpdateAddressBalance(db *sql.DB, addressID int64, balance float64) error {
	_, err := db.Exec(`
		UPDATE tracked_addresses 
		SET balance = $1, updated_at = CURRENT_TIMESTAMP 
		WHERE id = $2
	`, balance, addressID)
	return err
}

// Add transaction
func AddTransaction(db *sql.DB, tx *Transaction) error {
	_, err := db.Exec(`
		INSERT INTO transactions (address_id, tx_id, block_hash, block_height, amount, is_incoming, confirmations)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (address_id, tx_id) DO UPDATE 
		SET block_hash = $3, block_height = $4, confirmations = $7
	`, tx.AddressID, tx.TxID, tx.BlockHash, tx.BlockHeight, tx.Amount, tx.IsIncoming, tx.Confirmations)
	return err
}

// Add unspent output
func AddUnspentOutput(db *sql.DB, output *UnspentOutput) error {
	_, err := db.Exec(`
		INSERT INTO unspent_outputs (address_id, tx_id, vout, amount, script)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (address_id, tx_id, vout) DO UPDATE 
		SET amount = $4, script = $5
	`, output.AddressID, output.TxID, output.Vout, output.Amount, output.Script)
	return err
}

// Remove unspent output (when spent)
func RemoveUnspentOutput(db *sql.DB, addressID int64, txID string, vout int) error {
	_, err := db.Exec(`
		DELETE FROM unspent_outputs 
		WHERE address_id = $1 AND tx_id = $2 AND vout = $3
	`, addressID, txID, vout)
	return err
}

// GetLastProcessedBlock returns the last processed block height and hash
func GetLastProcessedBlock(db *sql.DB) (height int64, hash string, err error) {
	err = db.QueryRow(`
		SELECT last_block_height, last_block_hash 
		FROM block_tracker 
		WHERE id = 1
	`).Scan(&height, &hash)

	if err == sql.ErrNoRows {
		// If no record exists, return 0 height and empty hash
		return 0, "", nil
	}

	return height, hash, err
}

// UpdateLastProcessedBlock updates the last processed block height and hash
func UpdateLastProcessedBlock(db *sql.DB, height int64, hash string) error {
	_, err := db.Exec(`
		INSERT INTO block_tracker (id, last_block_height, last_block_hash, updated_at)
		VALUES (1, $1, $2, CURRENT_TIMESTAMP)
		ON CONFLICT (id) DO UPDATE 
		SET last_block_height = $1, last_block_hash = $2, updated_at = CURRENT_TIMESTAMP
	`, height, hash)
	return err
}
