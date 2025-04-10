package doge

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/mr-tron/base58"
)

const (
	// Protocol version
	ProtocolVersion = 70015

	// MaxMessageSize is the maximum allowed size for a message
	MaxMessageSize = 32 * 1024 * 1024 // 32MB

	// MaxHeadersResults is the maximum number of headers to request in one getheaders message
	MaxHeadersResults = 1000
)

// Deserialize reads a block header from a byte slice
func (h *BlockHeader) Deserialize(data []byte) error {
	if len(data) < 80 {
		return fmt.Errorf("header data too short")
	}

	h.Version = binary.LittleEndian.Uint32(data[0:4])
	copy(h.PrevBlock[:], data[4:36])
	copy(h.MerkleRoot[:], data[36:68])
	h.Time = binary.LittleEndian.Uint32(data[68:72])
	h.Bits = binary.LittleEndian.Uint32(data[72:76])
	h.Nonce = binary.LittleEndian.Uint32(data[76:80])

	return nil
}

// SPVNode represents a Simplified Payment Verification node
type SPVNode struct {
	headers            map[uint32]BlockHeader
	blocks             map[string]*Block
	peers              []string
	watchAddresses     map[string]bool
	bloomFilter        []byte
	currentHeight      uint32
	verackReceived     chan struct{}
	db                 BlockDatabase
	logger             *log.Logger
	connected          bool
	lastMessage        time.Time
	messageTimeout     time.Duration
	chainParams        *ChainParams
	conn               net.Conn
	stopChan           chan struct{}
	reconnectDelay     time.Duration
	bestKnownHeight    uint32
	chainTip           *BlockHeader
	headerSyncComplete bool
	blockSyncComplete  bool
	startHeight        uint32
	headersMutex       sync.RWMutex
	blockChan          chan *Block
	targetHeight       uint32
	done               chan struct{}
	mu                 sync.Mutex
	// Additional fields from reference implementation
	nonce     uint64
	userAgent string
	services  uint64
	relay     bool
	version   int32
}

// NewSPVNode creates a new SPV node
func NewSPVNode(peers []string, startHeight uint32, db BlockDatabase, logger *log.Logger) (*SPVNode, error) {
	// Initialize chain params with Dogecoin mainnet parameters
	chainParams := &ChainParams{
		ChainName:    "mainnet",
		GenesisBlock: "1a91e3dace36e2be3bf030a65679fe821aa1d6ef92e7c9902eb318182c355691",
		DefaultPort:  22556,
		RPCPort:      22555,
		DNSSeeds:     []string{"seed.dogecoin.com", "seed.multidoge.org", "seed.dogechain.info"},
		Checkpoints:  make(map[int]string),
	}

	// Create SPV node
	node := &SPVNode{
		peers:              peers,
		headers:            make(map[uint32]BlockHeader),
		blocks:             make(map[string]*Block),
		currentHeight:      uint32(startHeight),
		startHeight:        uint32(startHeight),
		bestKnownHeight:    0,
		db:                 db,
		verackReceived:     make(chan struct{}),
		headerSyncComplete: false,
		blockSyncComplete:  false,
		logger:             logger,
		chainParams:        chainParams,
		stopChan:           make(chan struct{}),
		reconnectDelay:     5 * time.Second,
		blockChan:          make(chan *Block),
		targetHeight:       uint32(startHeight),
		done:               make(chan struct{}),
	}

	// Load existing headers from database
	if err := node.loadHeadersFromDB(); err != nil {
		log.Printf("Error loading headers from database: %v", err)
	}

	return node, nil
}

// loadHeadersFromDB loads headers from the database
func (n *SPVNode) loadHeadersFromDB() error {
	// Get all headers from database
	headers, err := n.db.GetHeaders()
	if err != nil {
		return fmt.Errorf("error getting headers from database: %v", err)
	}

	// Store headers in memory
	n.headersMutex.Lock()
	defer n.headersMutex.Unlock()
	for _, header := range headers {
		n.headers[header.Height] = *header
		if header.Height > n.currentHeight {
			n.currentHeight = header.Height
		}
	}

	n.logger.Printf("Loaded %d headers from database, current height: %d", len(headers), n.currentHeight)
	return nil
}

// Connect establishes a connection to a Dogecoin node
func (n *SPVNode) Connect(addr string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}

	n.conn = conn
	n.logger.Printf("Connected to %s", addr)
	return nil
}

// Start begins the SPV node's operation
func (n *SPVNode) Start() error {
	n.logger.Println("Starting SPV node...")

	// Initialize message handling
	go n.handleMessages()

	// Send version message using the implementation from messages.go
	if err := n.sendVersionMessage(); err != nil {
		return fmt.Errorf("error sending version message: %v", err)
	}
	n.logger.Printf("Sent version message")

	// Wait for version handshake to complete
	select {
	case <-n.verackReceived:
		n.logger.Println("Version handshake completed")
		// Start header synchronization
		if err := n.startHeaderSync(); err != nil {
			return fmt.Errorf("error starting header sync: %v", err)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for version handshake")
	}

	return nil
}

// startHeaderSync starts the header synchronization process
func (n *SPVNode) startHeaderSync() error {
	// Get the last known header from database
	lastHash, height, _, err := n.db.GetLastProcessedBlock()
	if err != nil {
		return fmt.Errorf("error getting last processed block: %v", err)
	}

	// If we have no headers, start from genesis
	if height == 0 {
		lastHash = "1a91e3dace36e2be3bf030a65679fe821aa1d6ef92e7c9902eb318182c355691"
		n.logger.Printf("Starting from genesis block: %s", lastHash)
	} else {
		n.logger.Printf("Starting from block %s at height %d", lastHash, height)
	}

	// Send getheaders message
	if err := n.sendGetHeaders(lastHash); err != nil {
		return fmt.Errorf("error sending getheaders: %v", err)
	}

	n.logger.Printf("Requested headers starting from block %s at height %d", lastHash, height)
	return nil
}

// Done returns a channel that is closed when the node stops
func (n *SPVNode) Done() <-chan struct{} {
	return n.done
}

// Stop gracefully shuts down the SPV node
func (n *SPVNode) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.conn != nil {
		n.conn.Close()
	}
	close(n.done)
}

// handleMessages handles incoming messages from the peer
func (n *SPVNode) handleMessages() {
	n.logger.Printf("Starting message handler")
	for {
		n.logger.Printf("Waiting for message...")
		msg, err := n.readMessage()
		if err != nil {
			if err == io.EOF {
				n.logger.Printf("Connection closed by peer")
			} else {
				n.logger.Printf("Error reading message: %v", err)
			}
			return
		}

		command := string(bytes.TrimRight(msg.Command[:], "\x00"))
		n.logger.Printf("Received message of type: %s (length: %d)", command, msg.Length)

		switch command {
		case MsgVersion:
			n.logger.Printf("Received version message")
			if err := n.handleVersionMessage(msg.Payload); err != nil {
				n.logger.Printf("Error handling version message: %v", err)
				return
			}
		case MsgVerack:
			n.logger.Printf("Received verack message")
			select {
			case n.verackReceived <- struct{}{}:
				n.logger.Printf("Sent verack signal")
			default:
				n.logger.Printf("No one waiting for verack")
			}
		case MsgHeaders:
			n.logger.Printf("Received headers message (length: %d)", len(msg.Payload))
			if err := n.handleHeadersMessage(msg); err != nil {
				n.logger.Printf("Error handling headers message: %v", err)
			}
		case MsgBlock:
			if err := n.handleBlockMessage(msg.Payload); err != nil {
				n.logger.Printf("Error handling block message: %v", err)
			}
		case MsgTx:
			if err := n.handleTxMessage(msg); err != nil {
				n.logger.Printf("Error handling transaction message: %v", err)
			}
		case MsgInv:
			if err := n.handleInvMessage(msg.Payload); err != nil {
				n.logger.Printf("Error handling inventory message: %v", err)
			}
		case MsgPing:
			if err := n.handlePingMessage(msg.Payload); err != nil {
				n.logger.Printf("Error handling ping message: %v", err)
			}
		case "sendheaders":
			n.logger.Printf("Received sendheaders message")
		case "sendcmpct":
			n.logger.Printf("Received sendcmpct message")
		case "getheaders":
			n.logger.Printf("Received getheaders message")
			if err := n.handleGetHeadersMessage(msg.Payload); err != nil {
				n.logger.Printf("Error handling getheaders message: %v", err)
			}
		case "feefilter":
			n.logger.Printf("Received feefilter message")
		default:
			n.logger.Printf("Received unknown message type: %s", command)
		}
	}
}

// handleGetHeadersMessage handles a getheaders message from the peer
func (n *SPVNode) handleGetHeadersMessage(payload []byte) error {
	reader := bytes.NewReader(payload)

	// Read version (4 bytes)
	var version uint32
	if err := binary.Read(reader, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("error reading version: %v", err)
	}

	// Read hash count (varint)
	hashCount, err := binary.ReadUvarint(reader)
	if err != nil {
		return fmt.Errorf("error reading hash count: %v", err)
	}

	// Read block locator hashes
	locatorHashes := make([][32]byte, hashCount)
	for i := uint64(0); i < hashCount; i++ {
		if _, err := reader.Read(locatorHashes[i][:]); err != nil {
			return fmt.Errorf("error reading locator hash: %v", err)
		}
	}

	// Read stop hash (32 bytes)
	var stopHash [32]byte
	if _, err := reader.Read(stopHash[:]); err != nil {
		return fmt.Errorf("error reading stop hash: %v", err)
	}

	// Find headers to send
	var headers []BlockHeader
	for _, hash := range locatorHashes {
		for _, header := range n.headers {
			if bytes.Equal(header.PrevBlock[:], hash[:]) {
				headers = append(headers, header)
			}
		}
	}

	// Send headers message
	return n.sendHeadersMessage(headers)
}

// sendHeadersMessage sends a headers message
func (n *SPVNode) sendHeadersMessage(headers []BlockHeader) error {
	// Create headers message payload
	payload := make([]byte, 0)

	// Headers count (varint)
	countBytes := make([]byte, binary.MaxVarintLen64)
	bytesWritten := binary.PutUvarint(countBytes, uint64(len(headers)))
	payload = append(payload, countBytes[:bytesWritten]...)

	// Each header
	for _, header := range headers {
		// Version (4 bytes)
		versionBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(versionBytes, header.Version)
		payload = append(payload, versionBytes...)

		// Previous block hash (32 bytes)
		payload = append(payload, header.PrevBlock[:]...)

		// Merkle root (32 bytes)
		payload = append(payload, header.MerkleRoot[:]...)

		// Time (4 bytes)
		timeBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(timeBytes, header.Time)
		payload = append(payload, timeBytes...)

		// Bits (4 bytes)
		bitsBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(bitsBytes, header.Bits)
		payload = append(payload, bitsBytes...)

		// Nonce (4 bytes)
		nonceBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(nonceBytes, header.Nonce)
		payload = append(payload, nonceBytes...)

		// Transaction count (varint) - should be 0 for headers message
		payload = append(payload, 0x00)
	}

	// Send message
	return n.sendMessage(NewMessage(MsgHeaders, payload))
}

// handleHeadersMessage processes incoming headers messages
func (n *SPVNode) handleHeadersMessage(msg *Message) error {
	// Read headers count
	count, err := ReadVarInt(msg.Payload)
	if err != nil {
		return fmt.Errorf("error reading headers count: %v", err)
	}

	if count == 0 {
		n.logger.Printf("No more headers to sync")
		n.headerSyncComplete = true
		return nil
	}

	n.logger.Printf("Received %d headers", count)

	// Process each header
	offset := 1 // Skip the count byte
	for i := uint64(0); i < count; i++ {
		if offset+80 > len(msg.Payload) {
			return fmt.Errorf("partial headers message received")
		}

		header := &BlockHeader{}
		headerData := msg.Payload[offset : offset+80]
		header.Version = binary.LittleEndian.Uint32(headerData[0:4])
		copy(header.PrevBlock[:], headerData[4:36])
		copy(header.MerkleRoot[:], headerData[36:68])
		header.Time = binary.LittleEndian.Uint32(headerData[68:72])
		header.Bits = binary.LittleEndian.Uint32(headerData[72:76])
		header.Nonce = binary.LittleEndian.Uint32(headerData[76:80])

		// Calculate block hash
		hash := header.GetHash()
		blockHash := hex.EncodeToString(hash[:])

		// Validate header
		if err := n.validateHeader(*header); err != nil {
			return fmt.Errorf("invalid header at height %d: %v", n.currentHeight+1, err)
		}

		// Update current height
		n.currentHeight++
		header.Height = uint32(n.currentHeight)

		// Store header in database
		if err := n.db.StoreBlock(&Block{Header: *header}); err != nil {
			return fmt.Errorf("error storing header: %v", err)
		}

		// Update chain tip
		n.chainTip = header

		n.logger.Printf("Processed header %d: %s", n.currentHeight, blockHash)

		offset += 80
	}

	// Request more headers if available
	if count == MaxHeadersResults {
		// Send getheaders message with the last header's hash
		lastHeader := n.chainTip
		hashBytes := lastHeader.GetHash()
		hash := hex.EncodeToString(hashBytes[:])
		if err := n.sendGetHeaders(hash); err != nil {
			return fmt.Errorf("error requesting more headers: %v", err)
		}
		n.logger.Printf("Requested more headers starting from block %s at height %d", hash, n.currentHeight)
	} else {
		n.headerSyncComplete = true
		n.logger.Printf("Header synchronization complete at height %d", n.currentHeight)
	}

	return nil
}

// handleBlockMessage handles a block message
func (n *SPVNode) handleBlockMessage(payload []byte) error {
	// Parse block message
	block, err := n.parseBlockMessage(payload)
	if err != nil {
		return fmt.Errorf("error parsing block message: %v", err)
	}

	// Calculate block hash
	headerBytes := block.Header.Serialize()
	hash1 := sha256.Sum256(headerBytes)
	hash2 := sha256.Sum256(hash1[:])
	blockHash := hex.EncodeToString(hash2[:])

	n.logger.Printf("Processing block %s at height %d", blockHash, block.Header.Height)

	// Log transactions
	for i, tx := range block.Transactions {
		n.logger.Printf("Transaction %d in block %s: %s", i, blockHash, tx.TxID)
	}

	return nil
}

// handleTxMessage processes incoming transaction messages
func (n *SPVNode) handleTxMessage(msg *Message) error {
	tx := &Transaction{}
	offset := 0

	// Read version
	if offset+4 > len(msg.Payload) {
		return fmt.Errorf("not enough bytes for version")
	}
	tx.Version = binary.LittleEndian.Uint32(msg.Payload[offset : offset+4])
	offset += 4

	// Read inputs
	inputCount, bytesRead := binary.Uvarint(msg.Payload[offset:])
	if bytesRead <= 0 {
		return fmt.Errorf("error reading input count")
	}
	offset += bytesRead

	for i := uint64(0); i < inputCount; i++ {
		var input TxInput
		if offset+36 > len(msg.Payload) {
			return fmt.Errorf("not enough bytes for input")
		}
		copy(input.PreviousOutput.Hash[:], msg.Payload[offset:offset+32])
		input.PreviousOutput.Index = binary.LittleEndian.Uint32(msg.Payload[offset+32 : offset+36])
		offset += 36

		// Read script length
		scriptLen, bytesRead := binary.Uvarint(msg.Payload[offset:])
		if bytesRead <= 0 {
			return fmt.Errorf("error reading script length")
		}
		offset += bytesRead

		// Read script
		if offset+int(scriptLen) > len(msg.Payload) {
			return fmt.Errorf("not enough bytes for script")
		}
		input.ScriptSig = make([]byte, scriptLen)
		copy(input.ScriptSig, msg.Payload[offset:offset+int(scriptLen)])
		offset += int(scriptLen)

		// Read sequence
		if offset+4 > len(msg.Payload) {
			return fmt.Errorf("not enough bytes for sequence")
		}
		input.Sequence = binary.LittleEndian.Uint32(msg.Payload[offset : offset+4])
		offset += 4

		tx.Inputs = append(tx.Inputs, input)
	}

	// Calculate transaction ID
	txBytes := msg.Payload[:offset]
	hash1 := sha256.Sum256(txBytes)
	hash2 := sha256.Sum256(hash1[:])
	tx.TxID = hex.EncodeToString(hash2[:])

	// Store transaction in database if relevant
	if n.isRelevantTransaction(tx) {
		n.logger.Printf("Found relevant transaction %s", tx.TxID)
		if err := n.db.StoreTransaction(tx, "", uint32(n.currentHeight)); err != nil {
			n.logger.Printf("Error storing transaction %s in database: %v", tx.TxID, err)
			return err
		}
	}

	return nil
}

// isRelevantTransaction checks if a transaction is relevant to the SPV node
func (n *SPVNode) isRelevantTransaction(tx *Transaction) bool {
	// Check if any output addresses match our watched addresses
	for _, output := range tx.Outputs {
		if n.isWatchedScript(output.ScriptPubKey) {
			return true
		}
	}
	return false
}

// isWatchedScript checks if a script corresponds to a watched address
func (n *SPVNode) isWatchedScript(script []byte) bool {
	if len(script) < 25 {
		return false
	}

	// Check for P2PKH script
	if script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 && script[23] == 0x88 && script[24] == 0xac {
		pubKeyHash := script[3:23]
		for addr := range n.watchAddresses {
			if bytes.Equal(pubKeyHash, []byte(addr)) {
				return true
			}
		}
	}

	return false
}

// handleInvMessage handles an inventory message
func (n *SPVNode) handleInvMessage(payload []byte) error {
	// Parse inventory count (varint)
	reader := bytes.NewReader(payload)
	count, err := binary.ReadUvarint(reader)
	if err != nil {
		return fmt.Errorf("error reading inventory count: %v", err)
	}
	log.Printf("Received inventory message with %d items", count)

	// Parse each inventory item
	for i := uint64(0); i < count; i++ {
		// Type (4 bytes)
		var invType uint32
		if err := binary.Read(reader, binary.LittleEndian, &invType); err != nil {
			return fmt.Errorf("error reading inventory type: %v", err)
		}

		// Hash (32 bytes)
		var hash [32]byte
		if _, err := reader.Read(hash[:]); err != nil {
			return fmt.Errorf("error reading inventory hash: %v", err)
		}

		// Convert hash to hex string
		hashStr := hex.EncodeToString(hash[:])

		switch invType {
		case 2: // MSG_BLOCK
			log.Printf("Received block inventory: %s", hashStr)
			// Request the block
			if err := n.sendGetDataMessage(invType, hash); err != nil {
				log.Printf("Error requesting block: %v", err)
			}
		case 1: // MSG_TX
			log.Printf("Received transaction inventory: %s", hashStr)
			// Request the transaction
			if err := n.sendGetDataMessage(invType, hash); err != nil {
				log.Printf("Error requesting transaction: %v", err)
			}
		default:
			log.Printf("Received unknown inventory type %d: %s", invType, hashStr)
		}
	}

	return nil
}

// handlePingMessage handles a ping message
func (n *SPVNode) handlePingMessage(payload []byte) error {
	// Send pong message with same nonce
	log.Printf("Received ping message, sending pong")
	return n.sendPongMessage(payload)
}

// AddWatchAddress adds an address to watch
func (n *SPVNode) AddWatchAddress(address string) {
	log.Printf("Adding address to watch list: %s", address)
	n.watchAddresses[address] = true
	n.updateBloomFilter()
}

// GetBlockCount returns the current block height
func (n *SPVNode) GetBlockCount() (int64, error) {
	if n.conn == nil {
		return 0, fmt.Errorf("not connected to peer")
	}
	// In a real implementation, this would query the peer for the current height
	// For now, return the current height from our headers
	n.headersMutex.RLock()
	defer n.headersMutex.RUnlock()

	var maxHeight uint32
	for height := range n.headers {
		if height > maxHeight {
			maxHeight = height
		}
	}
	n.logger.Printf("Current block height: %d", maxHeight)
	return int64(maxHeight), nil
}

// parseNetworkTransaction parses a transaction from the network protocol
func parseNetworkTransaction(payload []byte) (Transaction, int, error) {
	if len(payload) < 4 {
		return Transaction{}, 0, fmt.Errorf("transaction message too short")
	}

	tx := Transaction{
		Version: binary.LittleEndian.Uint32(payload[0:4]),
	}

	// Parse input count
	inputCount, n := binary.Uvarint(payload[4:])
	if n <= 0 {
		return Transaction{}, 0, fmt.Errorf("failed to parse input count")
	}
	offset := 4 + n

	// Parse inputs
	tx.Inputs = make([]TxInput, inputCount)
	for i := uint64(0); i < inputCount; i++ {
		if len(payload[offset:]) < 36 {
			return Transaction{}, 0, fmt.Errorf("input %d too short", i)
		}

		input := TxInput{
			PreviousOutput: OutPoint{
				Hash:  [32]byte{},
				Index: binary.LittleEndian.Uint32(payload[offset+32 : offset+36]),
			},
		}
		copy(input.PreviousOutput.Hash[:], payload[offset:offset+32])

		// Parse script length
		scriptLen, n := binary.Uvarint(payload[offset+36:])
		if n <= 0 {
			return Transaction{}, 0, fmt.Errorf("failed to parse script length for input %d", i)
		}
		offset += 36 + n

		// Parse script
		if len(payload[offset:]) < int(scriptLen) {
			return Transaction{}, 0, fmt.Errorf("script for input %d too short", i)
		}
		input.ScriptSig = make([]byte, scriptLen)
		copy(input.ScriptSig, payload[offset:offset+int(scriptLen)])
		offset += int(scriptLen)

		// Parse sequence
		if len(payload[offset:]) < 4 {
			return Transaction{}, 0, fmt.Errorf("sequence for input %d too short", i)
		}
		input.Sequence = binary.LittleEndian.Uint32(payload[offset : offset+4])
		offset += 4

		tx.Inputs[i] = input
	}

	// Parse output count
	outputCount, n := binary.Uvarint(payload[offset:])
	if n <= 0 {
		return Transaction{}, 0, fmt.Errorf("failed to parse output count")
	}
	offset += n

	// Parse outputs
	tx.Outputs = make([]TxOutput, outputCount)
	for i := uint64(0); i < outputCount; i++ {
		if len(payload[offset:]) < 8 {
			return Transaction{}, 0, fmt.Errorf("output %d too short", i)
		}

		output := TxOutput{
			Value: binary.LittleEndian.Uint64(payload[offset : offset+8]),
		}
		offset += 8

		// Parse script length
		scriptLen, n := binary.Uvarint(payload[offset:])
		if n <= 0 {
			return Transaction{}, 0, fmt.Errorf("failed to parse script length for output %d", i)
		}
		offset += n

		// Parse script
		if len(payload[offset:]) < int(scriptLen) {
			return Transaction{}, 0, fmt.Errorf("script for output %d too short", i)
		}
		output.ScriptPubKey = make([]byte, scriptLen)
		copy(output.ScriptPubKey, payload[offset:offset+int(scriptLen)])
		offset += int(scriptLen)

		tx.Outputs[i] = output
	}

	// Parse lock time
	if len(payload[offset:]) < 4 {
		return Transaction{}, 0, fmt.Errorf("lock time too short")
	}
	tx.LockTime = binary.LittleEndian.Uint32(payload[offset : offset+4])
	offset += 4

	// Calculate TxID (double SHA-256 of the serialized transaction)
	hash1 := sha256.Sum256(payload[:offset])
	hash2 := sha256.Sum256(hash1[:])
	tx.TxID = hex.EncodeToString(hash2[:])

	return tx, offset, nil
}

// isRelevant checks if a transaction is relevant to our watch addresses
func (n *SPVNode) isRelevant(tx *Transaction) bool {
	// Check if any of our watch addresses are in the transaction
	for _, output := range tx.Outputs {
		scriptHash := sha256.Sum256(output.ScriptPubKey)
		if n.bloomFilter != nil {
			// Check if the script hash matches our bloom filter
			if bytes.Contains(n.bloomFilter, scriptHash[:]) {
				return true
			}
		}
	}
	return false
}

// parseBlockMessage parses a block message from the network protocol
func (n *SPVNode) parseBlockMessage(payload []byte) (*Block, error) {
	if len(payload) < 80 {
		return nil, fmt.Errorf("block message too short")
	}

	block := &Block{
		Header: BlockHeader{
			Version:    binary.LittleEndian.Uint32(payload[0:4]),
			PrevBlock:  [32]byte{},
			MerkleRoot: [32]byte{},
			Time:       binary.LittleEndian.Uint32(payload[68:72]),
			Bits:       binary.LittleEndian.Uint32(payload[72:76]),
			Nonce:      binary.LittleEndian.Uint32(payload[76:80]),
		},
		Transactions: make([]*Transaction, 0),
	}

	copy(block.Header.PrevBlock[:], payload[4:36])
	copy(block.Header.MerkleRoot[:], payload[36:68])

	// Parse transaction count
	txCount, bytesRead := binary.Uvarint(payload[80:])
	if bytesRead <= 0 {
		return nil, fmt.Errorf("failed to parse transaction count")
	}

	// Parse transactions
	offset := 80 + bytesRead
	for i := uint64(0); i < txCount; i++ {
		tx, bytesRead, err := parseNetworkTransaction(payload[offset:])
		if err != nil {
			return nil, fmt.Errorf("failed to parse transaction %d: %v", i, err)
		}
		block.Transactions = append(block.Transactions, &tx)
		offset += bytesRead
	}

	return block, nil
}

// GetBlockTransactions retrieves transactions for a block
func (n *SPVNode) GetBlockTransactions(blockHash string) ([]*Transaction, error) {
	// Convert block hash to bytes
	hashBytes, err := hex.DecodeString(blockHash)
	if err != nil {
		return nil, fmt.Errorf("error decoding block hash: %v", err)
	}
	if len(hashBytes) != 32 {
		return nil, fmt.Errorf("invalid block hash length: expected 32 bytes, got %d", len(hashBytes))
	}

	// Create getdata message payload
	payload := make([]byte, 37) // 1 byte for type + 32 bytes for hash
	payload[0] = 2              // Set inventory type to block (2)
	copy(payload[1:], hashBytes)

	// Send message
	err = n.sendMessage(NewMessage(MsgGetData, payload))
	if err != nil {
		return nil, fmt.Errorf("error sending getdata message: %v", err)
	}

	// Wait for block message
	select {
	case blockMsg := <-n.blockChan:
		return blockMsg.Transactions, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for block message")
	}
}

// Internal functions

func (n *SPVNode) updateBloomFilter() {
	log.Printf("Updating bloom filter with %d watched addresses", len(n.watchAddresses))
	// In a real implementation, this would:
	// 1. Create a new bloom filter with appropriate size and false positive rate
	// 2. Add all watched addresses to the filter
	// 3. Send filterload message to peer
	n.bloomFilter = make([]byte, 256) // Placeholder implementation
}

// Helper functions

func extractAddressesFromScript(script []byte) []string {
	log.Printf("Extracting addresses from script of length %d", len(script))
	// In a real implementation, this would:
	// 1. Parse the script
	// 2. Extract P2PKH, P2SH, and other address types
	// 3. Convert to base58 addresses
	return []string{} // Placeholder implementation
}

// Message handling functions (to be implemented)

// Network protocol functions (to be implemented)

func (n *SPVNode) sendGetDataMessage(invType uint32, hash [32]byte) error {
	payload := make([]byte, 37)
	binary.LittleEndian.PutUint32(payload[0:4], 1) // Count
	binary.LittleEndian.PutUint32(payload[4:8], invType)
	copy(payload[8:40], hash[:])
	return n.sendMessage(NewMessage(MsgGetData, payload))
}

func (n *SPVNode) sendMemPool() error {
	log.Printf("Sending mempool message")
	return nil
}

// Merkle block verification (to be implemented)

func (n *SPVNode) verifyMerkleProof(header BlockHeader, txid [32]byte, proof []byte) bool {
	log.Printf("Verifying merkle proof for transaction %x", txid)
	return false
}

// Chain validation functions (to be implemented)

func (n *SPVNode) validateHeader(header BlockHeader) error {
	// Check version
	if header.Version < 1 {
		return fmt.Errorf("invalid version %d", header.Version)
	}

	// Check timestamp (must not be more than 2 hours in the future)
	maxTime := time.Now().Add(2 * time.Hour).Unix()
	if int64(header.Time) > maxTime {
		return fmt.Errorf("header timestamp too far in the future")
	}

	// Check previous block hash
	if n.currentHeight > 0 {
		prevHeader, err := n.db.GetBlock(hex.EncodeToString(header.PrevBlock[:]))
		if err != nil {
			return fmt.Errorf("error getting previous block: %v", err)
		}
		if prevHeader == nil {
			return fmt.Errorf("previous block not found")
		}
	}

	// Check proof of work
	target := CompactToBig(header.Bits)
	hash := header.GetHash()
	hashInt := new(big.Int).SetBytes(hash[:])
	if hashInt.Cmp(target) > 0 {
		return fmt.Errorf("block hash %x does not meet target difficulty", hash)
	}

	return nil
}

func (n *SPVNode) validateChain() error {
	log.Printf("Validating chain with %d headers", len(n.headers))
	return nil
}

// addressToScriptHash converts a Dogecoin address to its script hash
func (n *SPVNode) addressToScriptHash(address string) []byte {
	// TODO: Implement address to script hash conversion
	return []byte{}
}

// ExtractAddresses extracts addresses from a script
func (n *SPVNode) ExtractAddresses(script []byte) []string {
	addresses := make([]string, 0)

	// Check script length
	if len(script) < 23 { // Minimum length for P2PKH
		return addresses
	}

	// Check for P2PKH (Pay to Public Key Hash)
	// Format: OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
	if len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 && script[23] == 0x88 && script[24] == 0xac {
		pubKeyHash := script[3:23]
		// Convert to base58check with version byte 0x1E (Dogecoin P2PKH)
		version := []byte{0x1E}
		data := append(version, pubKeyHash...)
		hash1 := sha256.Sum256(data)
		hash2 := sha256.Sum256(hash1[:])
		checksum := hash2[:4]
		final := append(data, checksum...)
		address := Base58CheckEncode(final, 0x1E)
		addresses = append(addresses, address)
		return addresses
	}

	// Check for P2SH (Pay to Script Hash)
	// Format: OP_HASH160 <20 bytes> OP_EQUAL
	if len(script) == 23 && script[0] == 0xa9 && script[1] == 0x14 && script[22] == 0x87 {
		scriptHash := script[2:22]
		// Convert to base58check with version byte 0x16 (Dogecoin P2SH)
		version := []byte{0x16}
		data := append(version, scriptHash...)
		hash1 := sha256.Sum256(data)
		hash2 := sha256.Sum256(hash1[:])
		checksum := hash2[:4]
		final := append(data, checksum...)
		address := Base58CheckEncode(final, 0x16)
		addresses = append(addresses, address)
		return addresses
	}

	return addresses
}

// Base58CheckEncode encodes a byte slice with version prefix in base58 with checksum
func Base58CheckEncode(input []byte, version byte) string {
	b := make([]byte, 0, 1+len(input)+4)
	b = append(b, version)
	b = append(b, input...)
	cksum := checksum(b)
	b = append(b, cksum[:]...)
	return base58.Encode(b)
}

// checksum returns the first four bytes of the double SHA256 hash
func checksum(input []byte) [4]byte {
	h := sha256.Sum256(input)
	h2 := sha256.Sum256(h[:])
	var cksum [4]byte
	copy(cksum[:], h2[:4])
	return cksum
}

// GetBlockHeader gets a block header from the network
func (n *SPVNode) GetBlockHeader(blockHash string) (*BlockHeader, error) {
	if n.conn == nil {
		return nil, fmt.Errorf("not connected to peer")
	}

	// Create getheaders message
	msg := make([]byte, 0)

	// Command (12 bytes)
	command := "getheaders"
	msg = append(msg, []byte(command)...)
	msg = append(msg, make([]byte, 12-len(command))...)

	// Version (4 bytes)
	version := uint32(70015)
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, version)
	msg = append(msg, buf...)

	// Hash count (varint)
	msg = append(msg, byte(1))

	// Block hash (32 bytes)
	hashBytes, err := hex.DecodeString(blockHash)
	if err != nil {
		return nil, fmt.Errorf("invalid block hash: %v", err)
	}
	msg = append(msg, hashBytes...)

	// Stop hash (32 bytes)
	stopHash := make([]byte, 32)
	msg = append(msg, stopHash...)

	// Send message
	if err := binary.Write(n.conn, binary.LittleEndian, msg); err != nil {
		return nil, fmt.Errorf("failed to send getheaders message: %v", err)
	}

	// Read headers message
	// Note: In a real implementation, you would need to:
	// 1. Read the message header
	// 2. Read the headers data
	// 3. Parse the headers
	// 4. Return the requested header

	// For now, return a dummy header
	return &BlockHeader{
		Version: 70015,
		Time:    uint32(time.Now().Unix()),
		Bits:    0x1e0ffff0,
		Nonce:   0,
		Height:  0,
	}, nil
}

// GetBlockHash returns the hash of the block at the given height
func (n *SPVNode) GetBlockHash(height int64) (string, error) {
	// TODO: Implement block hash retrieval
	return "", nil
}

// ProcessTransaction checks if a transaction is relevant to our watched addresses
func (n *SPVNode) ProcessTransaction(tx *Transaction) bool {
	n.logger.Printf("Processing transaction")
	for _, output := range tx.Outputs {
		// Extract addresses from output script
		addresses := extractAddressesFromScript(output.ScriptPubKey)
		for _, addr := range addresses {
			if n.watchAddresses[addr] {
				n.logger.Printf("Found relevant transaction for watched address %s", addr)
				return true
			}
		}
	}
	return false
}

// StartConnectionManager manages the connection to peers
func (n *SPVNode) StartConnectionManager() {
	go func() {
		for {
			select {
			case <-n.stopChan:
				return
			default:
				if !n.connected {
					n.logger.Printf("Not connected, attempting to connect to peers...")
					for _, peer := range n.peers {
						if err := n.Connect(peer); err != nil {
							n.logger.Printf("Failed to connect to peer %s: %v", peer, err)
							continue
						}
						break
					}
				}
				time.Sleep(n.reconnectDelay)
			}
		}
	}()
}

// sendGetBlocks sends a getblocks message to request blocks
func (n *SPVNode) sendGetBlocks() error {
	// Create getblocks message payload
	payload := make([]byte, 0)

	// Version (4 bytes)
	versionBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(versionBytes, ProtocolVersion)
	payload = append(payload, versionBytes...)

	// Hash count (varint)
	payload = append(payload, 0x01) // One hash

	// Block locator hashes (32 bytes)
	// Start with the block at current height
	if n.currentHeight > 0 {
		// Find the block hash at current height
		n.headersMutex.RLock()
		header, exists := n.headers[n.currentHeight]
		n.headersMutex.RUnlock()

		if exists {
			// Calculate hash of the header
			headerBytes := header.Serialize()
			hash1 := sha256.Sum256(headerBytes)
			hash2 := sha256.Sum256(hash1[:])
			payload = append(payload, hash2[:]...)
		} else {
			// If we don't have the header, use genesis block hash
			genesisHash, err := hex.DecodeString(n.chainParams.GenesisBlock)
			if err != nil {
				return fmt.Errorf("failed to decode genesis block hash: %v", err)
			}
			// Reverse the hash (Dogecoin uses little-endian)
			for i, j := 0, len(genesisHash)-1; i < j; i, j = i+1, j-1 {
				genesisHash[i], genesisHash[j] = genesisHash[j], genesisHash[i]
			}
			payload = append(payload, genesisHash...)
		}
	} else {
		// Start with genesis block hash
		genesisHash, err := hex.DecodeString(n.chainParams.GenesisBlock)
		if err != nil {
			return fmt.Errorf("failed to decode genesis block hash: %v", err)
		}
		// Reverse the hash (Dogecoin uses little-endian)
		for i, j := 0, len(genesisHash)-1; i < j; i, j = i+1, j-1 {
			genesisHash[i], genesisHash[j] = genesisHash[j], genesisHash[i]
		}
		payload = append(payload, genesisHash...)
	}

	// Stop hash (32 bytes) - all zeros to get all blocks
	stopHash := make([]byte, 32)
	payload = append(payload, stopHash...)

	n.logger.Printf("Sending getblocks message with payload length: %d", len(payload))
	n.logger.Printf("Requesting blocks starting from height %d", n.currentHeight)
	return n.sendMessage(NewMessage("getblocks", payload))
}

// Helper functions for message serialization
func uint32ToBytes(n uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, n)
	return b
}

func uint8ToBytes(n uint8) []byte {
	return []byte{n}
}

// ReadVarInt reads a variable length integer from a byte slice
func ReadVarInt(payload []byte) (uint64, error) {
	if len(payload) == 0 {
		return 0, fmt.Errorf("empty payload")
	}

	// Read the first byte to determine the format
	firstByte := payload[0]

	switch {
	case firstByte < 0xfd:
		return uint64(firstByte), nil
	case firstByte == 0xfd:
		if len(payload) < 3 {
			return 0, fmt.Errorf("payload too short for uint16")
		}
		return uint64(binary.LittleEndian.Uint16(payload[1:3])), nil
	case firstByte == 0xfe:
		if len(payload) < 5 {
			return 0, fmt.Errorf("payload too short for uint32")
		}
		return uint64(binary.LittleEndian.Uint32(payload[1:5])), nil
	case firstByte == 0xff:
		if len(payload) < 9 {
			return 0, fmt.Errorf("payload too short for uint64")
		}
		return binary.LittleEndian.Uint64(payload[1:9]), nil
	default:
		return 0, fmt.Errorf("invalid varint format")
	}
}

// NewMessage creates a new message with the given command and payload
func NewMessage(command string, payload []byte) *Message {
	msg := &Message{
		Payload: payload,
	}
	copy(msg.Command[:], command)
	return msg
}

func (n *SPVNode) sendPingMessage() error {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return n.sendMessage(NewMessage(MsgPing, nonce))
}

func (n *SPVNode) sendPongMessage(nonce []byte) error {
	return n.sendMessage(NewMessage(MsgPong, nonce))
}

// CompactToBig converts a compact representation of a target difficulty to a big.Int
func CompactToBig(compact uint32) *big.Int {
	mantissa := compact & 0x007fffff
	isNegative := compact&0x00800000 != 0
	exponent := uint(compact >> 24)

	var bn *big.Int
	if exponent <= 3 {
		mantissa >>= 8 * (3 - exponent)
		bn = big.NewInt(int64(mantissa))
	} else {
		bn = big.NewInt(int64(mantissa))
		bn.Lsh(bn, 8*(exponent-3))
	}

	if isNegative {
		bn = bn.Neg(bn)
	}

	return bn
}

// GetHash returns the hash of the block header
func (h *BlockHeader) GetHash() [32]byte {
	// Serialize header
	data := make([]byte, 80)
	binary.LittleEndian.PutUint32(data[0:4], h.Version)
	copy(data[4:36], h.PrevBlock[:])
	copy(data[36:68], h.MerkleRoot[:])
	binary.LittleEndian.PutUint32(data[68:72], h.Time)
	binary.LittleEndian.PutUint32(data[72:76], h.Bits)
	binary.LittleEndian.PutUint32(data[76:80], h.Nonce)

	// Double SHA-256
	hash1 := sha256.Sum256(data)
	hash2 := sha256.Sum256(hash1[:])
	return hash2
}
