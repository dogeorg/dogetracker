package main

/*
 * DogeTracker
 *
 * This code is based on the Dogecoin Foundation's DogeWalker project
 * (github.com/dogeorg/dogewalker) and has been modified to create
 * a transaction tracking system for Dogecoin addresses.
 */

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dogeorg/doge"
	_ "github.com/lib/pq"
	"github.com/qlpqlp/dogetracker/pkg/chaser"
	"github.com/qlpqlp/dogetracker/pkg/core"
	"github.com/qlpqlp/dogetracker/pkg/mempool"
	"github.com/qlpqlp/dogetracker/pkg/spec"
	"github.com/qlpqlp/dogetracker/pkg/tracker"
	"github.com/qlpqlp/dogetracker/server/api"
	serverdb "github.com/qlpqlp/dogetracker/server/db"
)

type Config struct {
	rpcHost   string
	rpcPort   int
	rpcUser   string
	rpcPass   string
	zmqHost   string
	zmqPort   int
	batchSize int

	// PostgreSQL configuration
	dbHost string
	dbPort int
	dbUser string
	dbPass string
	dbName string

	// API configuration
	apiPort    int
	apiToken   string
	startBlock string
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvIntOrDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

// ProcessBlockTransactions processes all transactions in a block and updates the database
func ProcessBlockTransactions(db *sql.DB, block *tracker.ChainBlock, blockchain spec.Blockchain) error {
	// Get all tracked addresses
	rows, err := db.Query(`SELECT id, address, required_confirmations FROM tracked_addresses`)
	if err != nil {
		return fmt.Errorf("failed to get tracked addresses: %v", err)
	}
	defer rows.Close()

	// Create a map of tracked addresses for quick lookup
	trackedAddrs := make(map[string]struct {
		id                    int64
		requiredConfirmations int
	})
	for rows.Next() {
		var id int64
		var addr string
		var requiredConfirmations int
		if err := rows.Scan(&id, &addr, &requiredConfirmations); err != nil {
			return fmt.Errorf("failed to scan tracked address: %v", err)
		}
		trackedAddrs[addr] = struct {
			id                    int64
			requiredConfirmations int
		}{id, requiredConfirmations}
	}

	// First, update confirmations for all existing transactions
	for _, addrInfo := range trackedAddrs {
		// Update all transactions for this address
		_, err = db.Exec(`
			UPDATE transactions 
			SET confirmations = CASE 
					WHEN block_height IS NOT NULL THEN 
						CASE 
							WHEN CAST($1 - block_height + 1 AS INTEGER) > 50 THEN 50
							ELSE CAST($1 - block_height + 1 AS INTEGER)
						END
					ELSE 0
				END,
				status = CASE 
					WHEN block_height IS NOT NULL AND 
						CASE 
							WHEN CAST($1 - block_height + 1 AS INTEGER) > 50 THEN 50
							ELSE CAST($1 - block_height + 1 AS INTEGER)
						END >= $3 THEN 'confirmed' 
					ELSE 'pending' 
				END
			WHERE address_id = $2
		`, block.Height, addrInfo.id, addrInfo.requiredConfirmations)
		if err != nil {
			log.Printf("Error updating transaction confirmations: %v", err)
		}
	}

	// Process each transaction in the block
	for _, tx := range block.Block.Tx {
		// Calculate transaction fee
		var fee float64
		if len(tx.VIn) > 0 {
			// Sum all inputs
			var totalInputs float64
			for _, vin := range tx.VIn {
				if len(vin.TxID) == 0 {
					continue // Skip coinbase transactions
				}
				prevTxData, err := blockchain.GetRawTransaction(doge.HexEncodeReversed(vin.TxID))
				if err != nil {
					continue
				}
				prevTxBytes, err := doge.HexDecode(prevTxData["hex"].(string))
				if err != nil {
					continue
				}
				prevTx := doge.DecodeTx(prevTxBytes)
				if int(vin.VOut) < len(prevTx.VOut) {
					totalInputs += float64(prevTx.VOut[vin.VOut].Value) / 1e8
				}
			}

			// Sum all outputs
			var totalOutputs float64
			for _, vout := range tx.VOut {
				totalOutputs += float64(vout.Value) / 1e8
			}

			// Fee is the difference between inputs and outputs
			fee = totalInputs - totalOutputs
		}

		// Get transaction timestamp from block header
		blockHeader, err := blockchain.GetBlockHeader(block.Hash)
		if err != nil {
			log.Printf("Error getting block header: %v", err)
			continue
		}
		timestamp := int64(blockHeader.Time)

		// Process outputs (incoming transactions)
		for i, vout := range tx.VOut {
			// Extract addresses from output script
			scriptType, addr := doge.ClassifyScript(vout.Script, &doge.DogeMainNetChain)
			if scriptType == "" {
				continue
			}

			if addrInfo, exists := trackedAddrs[string(addr)]; exists {
				// Found a tracked address in the output
				amount := float64(vout.Value) / 1e8 // Convert from satoshis to DOGE

				// Get sender address from inputs
				var senderAddress string
				if len(tx.VIn) > 0 && len(tx.VIn[0].TxID) > 0 {
					// Get the first input's previous transaction
					txIDHex := doge.HexEncodeReversed(tx.VIn[0].TxID)
					prevTxData, err := blockchain.GetRawTransaction(txIDHex)
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

				// Create unspent output record
				unspentOutput := &serverdb.UnspentOutput{
					AddressID: addrInfo.id,
					TxID:      tx.TxID,
					Vout:      i,
					Amount:    amount,
					Script:    doge.HexEncode(vout.Script),
				}

				// Add unspent output to database
				if err := serverdb.AddUnspentOutput(db, unspentOutput); err != nil {
					log.Printf("Error adding unspent output: %v", err)
				}

				// Check if this transaction already exists
				var existingTxID int64
				err := db.QueryRow(`
					SELECT id FROM transactions 
					WHERE address_id = $1 AND tx_id = $2
				`, addrInfo.id, tx.TxID).Scan(&existingTxID)

				if err == sql.ErrNoRows {
					// Calculate initial confirmations
					confirmations := 1 // First confirmation

					// Determine initial status
					status := "pending"
					if confirmations >= addrInfo.requiredConfirmations {
						status = "confirmed"
					}

					// Transaction doesn't exist, create a new one
					transaction := &serverdb.Transaction{
						AddressID:       addrInfo.id,
						TxID:            tx.TxID,
						BlockHash:       block.Hash,
						BlockHeight:     block.Height,
						Amount:          amount,
						Fee:             fee,
						Timestamp:       timestamp,
						IsIncoming:      true,
						Confirmations:   confirmations,
						Status:          status,
						SenderAddress:   senderAddress,
						ReceiverAddress: string(addr),
					}

					// Add transaction to database
					if err := serverdb.AddTransaction(db, transaction); err != nil {
						log.Printf("Error adding transaction: %v", err)
					}
				} else if err != nil {
					log.Printf("Error checking for existing transaction: %v", err)
				} else {
					// Transaction exists, update it with the new block information
					_, err = db.Exec(`
						UPDATE transactions 
						SET block_hash = $1, 
							block_height = $2, 
							fee = $3,
							timestamp = $4,
							confirmations = CASE 
								WHEN block_height IS NOT NULL THEN 
									CASE 
										WHEN CAST($2 - block_height + 1 AS INTEGER) > 50 THEN 50
										ELSE CAST($2 - block_height + 1 AS INTEGER)
									END
								ELSE 1
							END,
							status = CASE 
								WHEN block_height IS NOT NULL AND 
									CASE 
										WHEN CAST($2 - block_height + 1 AS INTEGER) > 50 THEN 50
										ELSE CAST($2 - block_height + 1 AS INTEGER)
									END >= $5 THEN 'confirmed' 
								ELSE 'pending' 
							END,
							sender_address = $6,
							receiver_address = $7
						WHERE id = $8
					`, block.Hash, block.Height, fee, timestamp, addrInfo.requiredConfirmations, senderAddress, string(addr), existingTxID)
					if err != nil {
						log.Printf("Error updating transaction: %v", err)
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
			prevTxData, err := blockchain.GetRawTransaction(txIDHex)
			if err != nil {
				continue
			}

			// Decode previous transaction
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

				if addrInfo, exists := trackedAddrs[string(addr)]; exists {
					// Found a tracked address in the input
					amount := -float64(prevOut.Value) / 1e8 // Negative for outgoing, convert from satoshis

					// Get receiver address from outputs
					var receiverAddress string
					if len(tx.VOut) > 0 {
						_, receiverAddr := doge.ClassifyScript(tx.VOut[0].Script, &doge.DogeMainNetChain)
						receiverAddress = string(receiverAddr)
					}

					// Remove the unspent output as it's now spent
					if err := serverdb.RemoveUnspentOutput(db, addrInfo.id, txIDHex, int(vin.VOut)); err != nil {
						log.Printf("Error removing unspent output: %v", err)
					}

					// Check if this transaction already exists
					var existingTxID int64
					err := db.QueryRow(`
						SELECT id FROM transactions 
						WHERE address_id = $1 AND tx_id = $2
					`, addrInfo.id, tx.TxID).Scan(&existingTxID)

					if err == sql.ErrNoRows {
						// Calculate initial confirmations
						confirmations := 1 // First confirmation

						// Determine initial status
						status := "pending"
						if confirmations >= addrInfo.requiredConfirmations {
							status = "confirmed"
						}

						// Transaction doesn't exist, create a new one
						transaction := &serverdb.Transaction{
							AddressID:       addrInfo.id,
							TxID:            tx.TxID,
							BlockHash:       block.Hash,
							BlockHeight:     block.Height,
							Amount:          amount,
							Fee:             fee,
							Timestamp:       timestamp,
							IsIncoming:      false,
							Confirmations:   confirmations,
							Status:          status,
							SenderAddress:   string(addr),
							ReceiverAddress: receiverAddress,
						}

						// Add transaction to database
						if err := serverdb.AddTransaction(db, transaction); err != nil {
							log.Printf("Error adding transaction: %v", err)
						}
					} else if err != nil {
						log.Printf("Error checking for existing transaction: %v", err)
					} else {
						// Transaction exists, update it with the new block information
						_, err = db.Exec(`
							UPDATE transactions 
							SET block_hash = $1, 
								block_height = $2, 
								fee = $3,
								timestamp = $4,
								confirmations = CASE 
									WHEN block_height IS NOT NULL THEN 
										CASE 
											WHEN CAST($2 - block_height + 1 AS INTEGER) > 50 THEN 50
											ELSE CAST($2 - block_height + 1 AS INTEGER)
										END
									ELSE 1
								END,
								status = CASE 
									WHEN block_height IS NOT NULL AND 
										CASE 
											WHEN CAST($2 - block_height + 1 AS INTEGER) > 50 THEN 50
											ELSE CAST($2 - block_height + 1 AS INTEGER)
										END >= $5 THEN 'confirmed' 
									ELSE 'pending' 
								END,
								sender_address = $6,
								receiver_address = $7
							WHERE id = $8
						`, block.Hash, block.Height, fee, timestamp, addrInfo.requiredConfirmations, string(addr), receiverAddress, existingTxID)
						if err != nil {
							log.Printf("Error updating transaction: %v", err)
						}
					}
				}
			}
		}
	}

	// Update balances for all tracked addresses
	for addr, addrInfo := range trackedAddrs {
		// Get address details including unspent outputs
		_, _, unspentOutputs, err := serverdb.GetAddressDetails(db, addr)
		if err != nil {
			log.Printf("Error getting address details: %v", err)
			continue
		}

		// Calculate balance from unspent outputs
		var balance float64
		for _, output := range unspentOutputs {
			balance += output.Amount
		}

		// Update address balance
		if err := serverdb.UpdateAddressBalance(db, addrInfo.id, balance); err != nil {
			log.Printf("Error updating address balance: %v", err)
		}
	}

	return nil
}

// HandleChainReorganization handles a chain reorganization by undoing transactions from invalid blocks
func HandleChainReorganization(db *sql.DB, undo *tracker.UndoForkBlocks) error {
	// Get all tracked addresses
	rows, err := db.Query(`SELECT id, address FROM tracked_addresses`)
	if err != nil {
		return fmt.Errorf("failed to get tracked addresses: %v", err)
	}
	defer rows.Close()

	// Create a map of tracked addresses for quick lookup
	trackedAddrs := make(map[string]int64)
	for rows.Next() {
		var id int64
		var addr string
		if err := rows.Scan(&id, &addr); err != nil {
			return fmt.Errorf("failed to scan tracked address: %v", err)
		}
		trackedAddrs[addr] = id
	}

	// Remove transactions from invalid blocks
	for _, blockHash := range undo.BlockHashes {
		// Get all transactions from this block for tracked addresses
		rows, err := db.Query(`
			SELECT t.id, t.address_id, t.tx_id, t.amount, t.is_incoming, u.id, u.tx_id, u.vout
			FROM transactions t
			LEFT JOIN unspent_outputs u ON t.address_id = u.address_id AND t.tx_id = u.tx_id
			WHERE t.block_hash = $1
		`, blockHash)
		if err != nil {
			log.Printf("Error querying transactions for block %s: %v", blockHash, err)
			continue
		}
		defer rows.Close()

		// Process each transaction
		for rows.Next() {
			var txID int64
			var addrID int64
			var txHash string
			var amount float64
			var isIncoming bool
			var unspentID sql.NullInt64
			var unspentTxID sql.NullString
			var unspentVout sql.NullInt64

			err := rows.Scan(&txID, &addrID, &txHash, &amount, &isIncoming, &unspentID, &unspentTxID, &unspentVout)
			if err != nil {
				log.Printf("Error scanning transaction: %v", err)
				continue
			}

			// If this is an incoming transaction, we need to remove the unspent output
			if isIncoming && unspentID.Valid {
				// Remove the unspent output
				if err := serverdb.RemoveUnspentOutput(db, addrID, unspentTxID.String, int(unspentVout.Int64)); err != nil {
					log.Printf("Error removing unspent output: %v", err)
				}
			}

			// Delete the transaction
			_, err = db.Exec(`DELETE FROM transactions WHERE id = $1`, txID)
			if err != nil {
				log.Printf("Error deleting transaction: %v", err)
			}
		}
	}

	// Update balances for all tracked addresses
	for addr, addrID := range trackedAddrs {
		// Get address details including transactions and unspent outputs
		_, _, unspentOutputs, err := serverdb.GetAddressDetails(db, addr)
		if err != nil {
			log.Printf("Error getting address details: %v", err)
			continue
		}

		// Calculate balance from unspent outputs
		var balance float64
		for _, output := range unspentOutputs {
			balance += output.Amount
		}

		// Update address balance
		if err := serverdb.UpdateAddressBalance(db, addrID, balance); err != nil {
			log.Printf("Error updating address balance: %v", err)
		}
	}

	return nil
}

func main() {
	// Define command line flags
	rpcHost := flag.String("rpc-host", getEnvOrDefault("DOGE_RPC_HOST", "127.0.0.1"), "Dogecoin RPC host")
	rpcPort := flag.Int("rpc-port", getEnvIntOrDefault("DOGE_RPC_PORT", 22555), "Dogecoin RPC port")
	rpcUser := flag.String("rpc-user", getEnvOrDefault("DOGE_RPC_USER", "dogecoin"), "Dogecoin RPC username")
	rpcPass := flag.String("rpc-pass", getEnvOrDefault("DOGE_RPC_PASS", "dogecoin"), "Dogecoin RPC password")
	zmqHost := flag.String("zmq-host", getEnvOrDefault("DOGE_ZMQ_HOST", "127.0.0.1"), "Dogecoin ZMQ host")
	zmqPort := flag.Int("zmq-port", getEnvIntOrDefault("DOGE_ZMQ_PORT", 28332), "Dogecoin ZMQ port")

	// PostgreSQL flags
	dbHost := flag.String("db-host", getEnvOrDefault("DB_HOST", "localhost"), "PostgreSQL host")
	dbPort := flag.Int("db-port", getEnvIntOrDefault("DB_PORT", 5432), "PostgreSQL port")
	dbUser := flag.String("db-user", getEnvOrDefault("DB_USER", "postgres"), "PostgreSQL username")
	dbPass := flag.String("db-pass", getEnvOrDefault("DB_PASS", "postgres"), "PostgreSQL password")
	dbName := flag.String("db-name", getEnvOrDefault("DB_NAME", "dogetracker"), "PostgreSQL database name")

	// API flags
	apiPort := flag.Int("api-port", getEnvIntOrDefault("API_PORT", 420), "API server port")
	apiToken := flag.String("api-token", getEnvOrDefault("API_TOKEN", ""), "API bearer token for authentication")
	startBlock := flag.String("start-block", getEnvOrDefault("START_BLOCK", "1a91e3dace36e2be3bf030a65679fe821aa1d6ef92e7c9902eb318182c355691"), "Starting block hash or height to begin processing from")

	// Parse command line flags
	flag.Parse()

	config := Config{
		rpcHost:    *rpcHost,
		rpcPort:    *rpcPort,
		rpcUser:    *rpcUser,
		rpcPass:    *rpcPass,
		zmqHost:    *zmqHost,
		zmqPort:    *zmqPort,
		dbHost:     *dbHost,
		dbPort:     *dbPort,
		dbUser:     *dbUser,
		dbPass:     *dbPass,
		dbName:     *dbName,
		apiPort:    *apiPort,
		apiToken:   *apiToken,
		startBlock: *startBlock,
	}

	// Connect to PostgreSQL
	dbConnStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		config.dbHost, config.dbPort, config.dbUser, config.dbPass, config.dbName)
	db, err := sql.Open("postgres", dbConnStr)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Initialize database schema
	if err := serverdb.InitDB(db); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	log.Printf("Connecting to Dogecoin node at %s:%d", config.rpcHost, config.rpcPort)

	// Core Node blockchain access.
	blockchain := core.NewCoreRPCClient(config.rpcHost, config.rpcPort, config.rpcUser, config.rpcPass)

	// Get last processed block from database
	lastBlockHash, lastBlockHeight, err := serverdb.GetLastProcessedBlock(db)
	if err != nil {
		log.Printf("Failed to get last processed block: %v", err)
		// If no database block exists, get the current best block
		lastBlockHash, err = blockchain.GetBestBlockHash()
		if err != nil {
			log.Printf("Failed to get best block hash: %v", err)
			lastBlockHash = *startBlock // Fall back to default block
		} else {
			log.Printf("No database block found, starting from current best block: %s", lastBlockHash)
		}
	} else {
		log.Printf("Found last processed block in database: %s (height: %d)", lastBlockHash, lastBlockHeight)
	}

	ctx, shutdown := context.WithCancel(context.Background())

	// Check if startBlock is a block height (numeric) or block hash
	var startBlockHash string
	if *startBlock == "" {
		// No start block provided, use the last processed block from database or current best block
		startBlockHash = lastBlockHash
		log.Printf("No start block specified, using %s", startBlockHash)
	} else if blockHeight, err := strconv.ParseInt(*startBlock, 10, 64); err == nil {
		// It's a block height, get the corresponding block hash
		startBlockHash, err = blockchain.GetBlockHash(blockHeight)
		if err != nil {
			log.Printf("Failed to get block hash for height %d: %v", blockHeight, err)
			startBlockHash = lastBlockHash // Fall back to last processed block
		} else {
			log.Printf("Starting from block height %d (hash: %s)", blockHeight, startBlockHash)
		}
	} else {
		// It's already a block hash
		startBlockHash = *startBlock
		log.Printf("Starting from block hash: %s", startBlockHash)
	}

	// Get tracked addresses from database
	rows, err := db.Query(`SELECT address FROM tracked_addresses`)
	if err != nil {
		log.Printf("Failed to get tracked addresses: %v", err)
	} else {
		defer rows.Close()

		var trackedAddresses []string
		for rows.Next() {
			var addr string
			if err := rows.Scan(&addr); err != nil {
				log.Printf("Error scanning tracked address: %v", err)
				continue
			}
			trackedAddresses = append(trackedAddresses, addr)
		}

		log.Printf("Found %d tracked addresses", len(trackedAddresses))

		// Initialize mempool tracker with actual tracked addresses
		mempoolTracker := mempool.NewMempoolTracker(blockchain, db, trackedAddresses)
		go mempoolTracker.Start(ctx)

		// Start API server with mempool tracker
		apiServer := api.NewServer(db, config.apiToken, mempoolTracker)
		go func() {
			log.Printf("Starting API server on port %d", config.apiPort)
			if err := apiServer.Start(config.apiPort); err != nil {
				log.Printf("API server error: %v", err)
			}
		}()
	}

	// Watch for new blocks.
	zmqTip, err := core.CoreZMQListener(ctx, config.zmqHost, config.zmqPort)
	if err != nil {
		log.Printf("CoreZMQListener: %v", err)
		os.Exit(1)
	}
	tipChanged := chaser.NewTipChaser(ctx, zmqTip, blockchain).Listen(1, true)

	// Walk the blockchain.
	blocks, err := tracker.WalkTheDoge(ctx, tracker.TrackerOptions{
		Chain:           &doge.DogeMainNetChain,
		ResumeFromBlock: startBlockHash,
		Client:          blockchain,
		TipChanged:      tipChanged,
	})
	if err != nil {
		log.Printf("WalkTheDoge: %v", err)
		os.Exit(1)
	}

	// Process blocks and update database
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-blocks:
				if b.Block != nil {
					log.Printf("Processing block: %v (%v)", b.Block.Hash, b.Block.Height)
					// Update last processed block in database
					if err := serverdb.UpdateLastProcessedBlock(db, b.Block.Hash, b.Block.Height); err != nil {
						log.Printf("Failed to update last processed block: %v", err)
					}

					// Process block transactions and update database
					if err := ProcessBlockTransactions(db, b.Block, blockchain); err != nil {
						log.Printf("Failed to process block transactions: %v", err)
					}
				} else {
					log.Printf("Undoing to: %v (%v)", b.Undo.ResumeFromBlock, b.Undo.LastValidHeight)

					// Handle chain reorganization
					if err := HandleChainReorganization(db, b.Undo); err != nil {
						log.Printf("Failed to handle chain reorganization: %v", err)
					}
				}
			}
		}
	}()

	// Hook ^C signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for {
			select {
			case sig := <-sigCh: // sigterm/sigint caught
				log.Printf("Caught %v signal, shutting down", sig)
				shutdown()
				continue
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for shutdown.
	<-ctx.Done()
}
